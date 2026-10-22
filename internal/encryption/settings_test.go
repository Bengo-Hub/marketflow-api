package encryption

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/providersetting"
)

// testKeys builds a provider with only a test env key (no DB key), so results are deterministic.
func testKeys(t *testing.T) *KeyProvider {
	t.Helper()
	k, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return NewKeyProvider(nil, k)
}

// A Meta-style token: alphanumeric, so it also decodes as base64.
const metaLikeToken = "EAABsbCS1iHgBAKZCZBZBZAZCpQZDZD0123456789abcdefABCDEF"

func TestSealSecret(t *testing.T) {
	ctx := context.Background()
	p := testKeys(t)

	sealed, enc := p.SealSecret(ctx, "page_token", metaLikeToken, true)
	if !enc || sealed == metaLikeToken {
		t.Fatalf("secret was not encrypted: enc=%v", enc)
	}
	if back, err := p.Decrypt(ctx, sealed); err != nil || back != metaLikeToken {
		t.Fatalf("sealed value does not decrypt back: %v", err)
	}
	if v, enc := p.SealSecret(ctx, "page_name", "My Shop", false); enc || v != "My Shop" {
		t.Fatalf("non-secret must be stored as-is: %q enc=%v", v, enc)
	}
	if v, enc := p.SealSecret(ctx, "refresh_token", "rt-123", false); !enc || v == "rt-123" {
		t.Fatal("a key listed in SecretKeys must be encrypted even when the caller does not flag it")
	}
	if v, enc := p.SealSecret(ctx, "page_token", "", true); enc || v != "" {
		t.Fatal("empty values stay empty and unencrypted")
	}
	var nilP *KeyProvider
	if v, enc := nilP.SealSecret(ctx, "page_token", "x", true); enc || v != "x" {
		t.Fatal("a nil provider must store as-is, not panic")
	}
	if v, enc := NewKeyProvider(nil, "").SealSecret(ctx, "page_token", "x", true); enc || v != "x" {
		t.Fatal("with no key configured the value is stored as-is (pre-existing behaviour)")
	}
}

func TestSettingValueWithoutDB(t *testing.T) {
	ctx := context.Background()
	p := testKeys(t)
	sealed, _ := p.SealSecret(ctx, "page_token", metaLikeToken, true)

	if v, err := p.SettingValue(ctx, nil, &ent.ProviderSetting{Key: "page_token", Value: sealed, IsEncrypted: true}); err != nil || v != metaLikeToken {
		t.Fatalf("encrypted row: v=%q err=%v", v, err)
	}
	// A plaintext legacy Meta token (looks like base64) must come back unchanged, never as "broken ciphertext".
	if v, err := p.SettingValue(ctx, nil, &ent.ProviderSetting{Key: "page_token", Value: metaLikeToken, IsSecret: true}); err != nil || v != metaLikeToken {
		t.Fatalf("legacy plaintext token misread: v=%q err=%v", v, err)
	}
	if v, err := p.SettingValue(ctx, nil, &ent.ProviderSetting{Key: "page_name", Value: "My Shop"}); err != nil || v != "My Shop" {
		t.Fatalf("plain field: v=%q err=%v", v, err)
	}
	if v, err := p.SettingValue(ctx, nil, nil); err != nil || v != "" {
		t.Fatal("nil row must return empty")
	}
	var nilP *KeyProvider
	if _, err := nilP.SettingValue(ctx, nil, &ent.ProviderSetting{Key: "page_token", Value: sealed, IsEncrypted: true}); err == nil {
		t.Fatal("an encrypted row with no provider must error, never return ciphertext")
	}
	if v, err := nilP.SettingValue(ctx, nil, &ent.ProviderSetting{Key: "page_name", Value: "My Shop"}); err != nil || v != "My Shop" {
		t.Fatal("a nil provider must still return plain values")
	}
	if _, err := testKeys(t).SettingValue(ctx, nil, &ent.ProviderSetting{Key: "page_token", Value: sealed, IsEncrypted: true}); err == nil {
		t.Fatal("a wrong key must fail, never return ciphertext")
	}
}

const testEncPostgresDSN = "postgres://postgres:postgres@localhost:5432/marketflow?sslmode=disable"

// Legacy plaintext secrets are re-sealed in place on first read, and a stale read never
// overwrites a newer value.
func TestSettingValueReSealsLegacyPlaintext(t *testing.T) {
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", testEncPostgresDSN)
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
	p := testKeys(t)

	tenant := "enc-test-" + uuid.NewString()
	// Cleanup touches only this test's own tenant id.
	t.Cleanup(func() { _, _ = db.ProviderSetting.Delete().Where(providersetting.TenantID(tenant)).Exec(ctx) })
	mk := func(key, value string, secret bool) *ent.ProviderSetting {
		row, err := db.ProviderSetting.Create().SetTenantID(tenant).SetProviderType("meta_ads").SetProviderName("page").
			SetKey(key).SetValue(value).SetIsSecret(secret).Save(ctx)
		if err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
		return row
	}

	legacy := mk("page_token", metaLikeToken, true)
	if v, err := p.SettingValue(ctx, db, legacy); err != nil || v != metaLikeToken {
		t.Fatalf("legacy read: v=%q err=%v", v, err)
	}
	after, err := db.ProviderSetting.Get(ctx, legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.IsEncrypted || after.Value == metaLikeToken {
		t.Fatal("legacy plaintext secret was not re-sealed in place")
	}
	if v, err := p.SettingValue(ctx, db, after); err != nil || v != metaLikeToken {
		t.Fatalf("re-sealed row does not read back: v=%q err=%v", v, err)
	}

	plain := mk("page_name", "My Shop", false)
	if _, err := p.SettingValue(ctx, db, plain); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.ProviderSetting.Get(ctx, plain.ID); got.IsEncrypted || got.Value != "My Shop" {
		t.Fatal("a non-secret field must never be rewritten")
	}

	// Stale read: the token was replaced (a reconnect) after this reader loaded the row.
	stale := mk("user_token", "old-token", true)
	if _, err := db.ProviderSetting.UpdateOneID(stale.ID).SetValue("new-token").Save(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := p.SettingValue(ctx, db, stale); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.ProviderSetting.Get(ctx, stale.ID); got.Value != "new-token" || got.IsEncrypted {
		t.Fatalf("a stale read overwrote a newer value: %+v", got)
	}
}
