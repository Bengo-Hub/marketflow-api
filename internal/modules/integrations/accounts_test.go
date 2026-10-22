package integrations

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"go.uber.org/zap"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
)

const testPostgresDSN = "postgres://postgres:postgres@localhost:5432/marketflow?sslmode=disable"

func TestMetaAccounts(t *testing.T) {
	pages := []metaPage{
		{ID: "p1", Name: "Shop", AccessToken: "FAKE-P1"},
		{ID: "p2", Name: "Clinic", AccessToken: "FAKE-P2"},
		{ID: ""},
	}
	pages[1].Instagram = &struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}{ID: "ig2", Username: "clinic_ke"}
	ads := []metaAdAccount{{ID: "act_9", Name: "Ads", Currency: "KES", AccountStatus: 1}, {ID: ""}}

	got := metaAccounts(pages, ads, "user-tok", []string{"pages_show_list"})
	if len(got) != 4 {
		t.Fatalf("want 2 pages, 1 instagram, 1 ad account; got %d: %+v", len(got), got)
	}
	byID := map[string]account{}
	for _, a := range got {
		byID[a.ExternalID] = a
	}
	if a := byID["ig2"]; a.Type != AccountInstagramBusiness || a.Token != "FAKE-P2" || a.Metadata["page_id"] != "p2" {
		t.Errorf("instagram account acts with its Page's token: %+v", a)
	}
	if a := byID["act_9"]; a.Type != AccountMetaAdAccount || a.Token != "user-tok" || a.Metadata["currency"] != "KES" {
		t.Errorf("ad account acts with the user token: %+v", a)
	}
	if a := byID["p1"]; a.Type != AccountFacebookPage || a.Token != "FAKE-P1" || len(a.Scopes) != 1 {
		t.Errorf("page: %+v", a)
	}
}

func TestSaveAndRevokeAccounts(t *testing.T) {
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", testPostgresDSN)
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		t.Skipf("local postgres unreachable: %v", err)
	}
	defer sqlDB.Close()
	db := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, sqlDB)))
	defer db.Close()
	ctx := context.Background()

	tid := uuid.New()
	if _, err := db.Tenant.Create().SetID(tid).SetName("CA " + tid.String()).SetSlug("ca-" + tid.String()).Save(ctx); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	// Cleanup touches only this test's own tenant id.
	t.Cleanup(func() {
		_, _ = db.ConnectedAccount.Delete().Where(connectedaccount.TenantID(tid)).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})

	// With no key, nothing is stored: tokens never land in plain text.
	noKey := &Handler{db: db, keys: encryption.NewKeyProvider(nil, ""), logger: zap.NewNop()}
	if err := noKey.saveAccounts(ctx, tid.String(), []account{{Platform: PlatformMeta, Type: AccountFacebookPage, ExternalID: "p1", Token: "tok"}}); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.ConnectedAccount.Query().Where(connectedaccount.TenantID(tid)).Count(ctx); n != 0 {
		t.Fatalf("rows written without a key: %d", n)
	}

	key, err := encryption.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	keys := encryption.NewKeyProvider(nil, key)
	h := &Handler{db: db, keys: keys, logger: zap.NewNop()}
	first := []account{
		{Platform: PlatformMeta, Type: AccountFacebookPage, ExternalID: "p1", DisplayName: "Shop", Token: "tok-1", Metadata: map[string]any{}},
		{Platform: PlatformMeta, Type: AccountMetaAdAccount, ExternalID: "act_9", DisplayName: "Ads", Token: "user-1", Metadata: map[string]any{"currency": "KES"}},
	}
	if err := h.saveAccounts(ctx, tid.String(), first); err != nil {
		t.Fatal(err)
	}
	// Reconnecting updates the same rows (new token, new name), never duplicates them.
	first[0].DisplayName, first[0].Token = "Shop KE", "tok-2"
	if err := h.saveAccounts(ctx, tid.String(), first); err != nil {
		t.Fatal(err)
	}
	rows, err := db.ConnectedAccount.Query().Where(connectedaccount.TenantID(tid)).All(ctx)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows = %d, err %v", len(rows), err)
	}
	page, _ := db.ConnectedAccount.Query().Where(connectedaccount.TenantID(tid), connectedaccount.ExternalID("p1")).Only(ctx)
	if page.TokenCiphertext == "" || page.TokenCiphertext == "tok-2" {
		t.Fatal("the token must be stored as ciphertext")
	}
	if tok, err := keys.Decrypt(ctx, page.TokenCiphertext); err != nil || tok != "tok-2" || page.DisplayName != "Shop KE" || page.Status != "active" {
		t.Fatalf("reconnect not applied: %q %v %+v", tok, err, page)
	}

	if n, err := h.revokeAccounts(ctx, tid.String(), "google_calendar"); err != nil || n != 0 {
		t.Fatalf("a provider without accounts revokes nothing: %d %v", n, err)
	}
	if n, err := h.revokeAccounts(ctx, tid.String(), "meta_ads"); err != nil || n != 2 {
		t.Fatalf("revoke: %d %v", n, err)
	}
	revoked, _ := db.ConnectedAccount.Query().Where(connectedaccount.TenantID(tid), connectedaccount.ExternalID("p1")).Only(ctx)
	if revoked.Status != "revoked" || revoked.TokenCiphertext != "" {
		t.Fatalf("a revoked account keeps no token: %+v", revoked)
	}
}
