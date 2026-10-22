package worker

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
)

func TestMetaTokenResolverPages(t *testing.T) {
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", testWorkerPostgresDSN)
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
	if _, err := db.Tenant.Create().SetID(tid).SetName("Tok " + tid.String()).SetSlug("tok-" + tid.String()).
		SetMetaPageID("P-DEF").SetMetaAccessToken("LEGACY").Save(ctx); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	// Cleanup touches only this test's own tenant id.
	t.Cleanup(func() {
		_, _ = db.ConnectedAccount.Delete().Where(connectedaccount.TenantID(tid)).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})

	key, err := encryption.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	keys := encryption.NewKeyProvider(nil, key)
	for page, tok := range map[string]string{"P-DEF": "CA-DEF", "P-2": "CA-2"} {
		sealed, err := keys.Encrypt(ctx, tok)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ConnectedAccount.Create().SetTenantID(tid).SetPlatform("meta").SetAccountType("facebook_page").
			SetExternalID(page).SetTokenCiphertext(sealed).Save(ctx); err != nil {
			t.Fatal(err)
		}
	}

	r := NewMetaTokenResolver(db, keys, zap.NewNop())
	if tok, ok := r.Resolve(ctx, tid.String()); !ok || tok != "CA-DEF" {
		t.Errorf("default page: %q %v, want the ConnectedAccount token", tok, ok)
	}
	if tok, ok := r.ResolvePage(ctx, tid.String(), "P-2"); !ok || tok != "CA-2" {
		t.Errorf("second page: %q %v", tok, ok)
	}
	if tok, ok := r.ResolvePage(ctx, tid.String(), "P-3"); ok {
		t.Errorf("an unknown page must not get the default page's token, got %q", tok)
	}
	if _, err := db.ConnectedAccount.Update().
		Where(connectedaccount.TenantID(tid), connectedaccount.ExternalID("P-2")).
		SetStatus("revoked").Save(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.ResolvePage(ctx, tid.String(), "P-2"); ok {
		t.Error("a revoked page has no token")
	}

	// Without keys the ConnectedAccount rows cannot be read: the legacy stores still serve
	// the default page.
	legacy := NewMetaTokenResolver(db, nil, zap.NewNop())
	if tok, ok := legacy.Resolve(ctx, tid.String()); !ok || tok != "LEGACY" {
		t.Errorf("legacy fallback: %q %v", tok, ok)
	}
}
