package learning

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/serviceconfig"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func testDSN() string {
	if v := os.Getenv("MF_TEST_POSTGRES_DSN"); v != "" {
		return v
	}
	// The same local database the other module tests use; rows are this test's own and removed.
	return "postgres://postgres:postgres@localhost:5432/marketflow?sslmode=disable"
}

func openDB(t *testing.T) *ent.Client {
	t.Helper()
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	db, err := sql.Open("pgx", testDSN())
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		t.Skipf("local postgres unreachable: %v", err)
	}
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func cleanup(t *testing.T, db *ent.Client, tenants ...uuid.UUID) {
	t.Cleanup(func() {
		_, _ = db.ServiceConfig.Delete().Where(serviceconfig.TenantIDIn(tenants...), serviceconfig.ConfigKeyHasPrefix(keyPrefix)).Exec(context.Background())
	})
}

func TestValidRefusesPersonalAndUnnormalised(t *testing.T) {
	for _, ok := range []string{"where is my team", "nionyeshe timu yangu", "show team 2"} {
		if !valid("team", ok) {
			t.Errorf("%q is a normal phrase", ok)
		}
	}
	for _, bad := range []string{"Where is my team", "mail me at a@b.co", "call 0712345678", "", "a  b",
		"one two three four five six seven eight nine ten eleven twelve thirteen"} {
		if valid("team", bad) {
			t.Errorf("%q must be refused", bad)
		}
	}
	if valid("Team!", "where is my team") {
		t.Error("intent must be a plain key")
	}
}

// A tenant's phrase is learned and counted; another tenant does not see it until three tenants
// use the same phrase.
func TestRecordAndShare(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	s := NewStore(db)
	a, b, c, d := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	cleanup(t, db, a, b, c, d)
	phrase := "where is my team " + uuid.NewString()[:8] // unique per run, digits and letters only
	phrase = normalizeForTest(phrase)

	for i := 0; i < 2; i++ {
		if err := s.Record(ctx, a, "team", phrase); err != nil {
			t.Fatal(err)
		}
	}
	own, _ := s.Phrases(ctx, a, "team")
	if !contains(own, phrase) {
		t.Fatalf("the tenant sees its own phrase: %v", own)
	}
	row := db.ServiceConfig.Query().Where(serviceconfig.TenantID(a), serviceconfig.ConfigKey(key("team", phrase))).OnlyX(ctx)
	if want := `"count":2`; !containsStr(row.ConfigValue, want) {
		t.Fatalf("a repeat is counted: %s", row.ConfigValue)
	}
	if other, _ := s.Phrases(ctx, d, "team"); contains(other, phrase) {
		t.Fatal("one tenant's phrase is not shared yet")
	}
	_ = s.Record(ctx, b, "team", phrase)
	_ = s.Record(ctx, c, "team", phrase)
	if other, _ := s.Phrases(ctx, d, "team"); !contains(other, phrase) {
		t.Fatalf("a phrase three tenants use is shared: %v", other)
	}
	if err := s.Record(ctx, a, "team", "Call me on 0712345678"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("personal text is refused: %v", err)
	}
}

func normalizeForTest(s string) string {
	out := []rune{}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || r == ' ' {
			out = append(out, r)
		}
	}
	return string(out)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestHasPersonalDetails(t *testing.T) {
	for _, s := range []string{"mail me at jane@example.com", "call 0712 345 678", "+254-712-345678", "id 12345678"} {
		if !HasPersonalDetails(s) {
			t.Errorf("%q should be personal", s)
		}
	}
	for _, s := range []string{"do you deliver to kisumu", "price for 2 kg", "open 8 to 5", "order 12345"} {
		if HasPersonalDetails(s) {
			t.Errorf("%q is not personal", s)
		}
	}
}
