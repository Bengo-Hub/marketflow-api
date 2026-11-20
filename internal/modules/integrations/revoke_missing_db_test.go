package integrations

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
)

const revokeTestDSN = "postgres://postgres:postgres@localhost:5432/marketflow?sslmode=disable"

func openRevokeDB(t *testing.T) *ent.Client {
	t.Helper()
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", revokeTestDSN)
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		t.Skipf("local postgres unreachable: %v", err)
	}
	db := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, sqlDB)))
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// An ad account the person removed from the business is retired on the next complete listing;
// the kept account, and other account types on the same platform, are left alone.
func TestRevokeMissingRetiresUngrantedAccounts(t *testing.T) {
	db := openRevokeDB(t)
	ctx := context.Background()
	tid := uuid.New()
	if _, err := db.Tenant.Create().SetID(tid).SetName("RM " + tid.String()).SetSlug("rm-" + tid.String()).Save(ctx); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() { _ = db.Tenant.DeleteOneID(tid).Exec(ctx) })
	mk := func(typ, ext string) *ent.ConnectedAccount {
		a, err := db.ConnectedAccount.Create().SetTenantID(tid).SetPlatform(PlatformMeta).SetAccountType(typ).
			SetExternalID(ext).SetDisplayName(ext).SetStatus("active").SetTokenCiphertext("enc:x").Save(ctx)
		if err != nil {
			t.Fatalf("seed account: %v", err)
		}
		t.Cleanup(func() { _ = db.ConnectedAccount.DeleteOneID(a.ID).Exec(ctx) })
		return a
	}
	keep := mk(AccountMetaAdAccount, "act_keep")
	gone := mk(AccountMetaAdAccount, "act_removed")
	page := mk(AccountFacebookPage, "page_1")

	h := &Handler{db: db, logger: zap.NewNop()}
	n, err := h.revokeMissing(ctx, tid.String(), PlatformMeta, []string{AccountMetaAdAccount}, []string{"act_keep"})
	if err != nil || n != 1 {
		t.Fatalf("revoked %d, %v", n, err)
	}
	if a, _ := db.ConnectedAccount.Get(ctx, gone.ID); a.Status != "revoked" || a.TokenCiphertext != "" {
		t.Fatalf("removed account not retired: %s %q", a.Status, a.TokenCiphertext)
	}
	if a, _ := db.ConnectedAccount.Get(ctx, keep.ID); a.Status != "active" {
		t.Fatalf("granted account touched: %s", a.Status)
	}
	if a, _ := db.ConnectedAccount.Get(ctx, page.ID); a.Status != "active" {
		t.Fatalf("other account type touched: %s", a.Status)
	}
	// Idempotent: nothing more to retire.
	if n, err := h.revokeMissing(ctx, tid.String(), PlatformMeta, []string{AccountMetaAdAccount}, []string{"act_keep"}); err != nil || n != 0 {
		t.Fatalf("second run revoked %d, %v", n, err)
	}
}

// Closed Meta ad accounts are not stored as usable accounts.
func TestMetaAccountsSkipClosedAdAccounts(t *testing.T) {
	got := metaAccounts(nil, []metaAdAccount{{ID: "act_1", AccountStatus: 1}, {ID: "act_2", AccountStatus: 101}, {ID: "act_3", AccountStatus: 100}}, "tok", nil, "")
	ids := externalIDsOf(got, AccountMetaAdAccount)
	if len(ids) != 1 || ids[0] != "act_1" {
		t.Fatalf("closed accounts kept: %v", ids)
	}
}
