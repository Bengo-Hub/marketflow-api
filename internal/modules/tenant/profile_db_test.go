package tenant

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/modules/marketingos"
)

func openProfileDB(t *testing.T) *ent.Client {
	t.Helper()
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", "postgres://postgres:postgres@localhost:5432/marketflow?sslmode=disable")
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

// A profile form that posts back a stale copy of the whole metadata must not undo a brand
// context saved in between, and only the sent keys change.
func TestUpdateProfileMergesMetadataKeys(t *testing.T) {
	db := openProfileDB(t)
	ctx := context.Background()
	tid := uuid.New()
	if _, err := db.Tenant.Create().SetID(tid).SetName("P " + tid.String()).SetSlug("p-" + tid.String()).
		SetMetadata(map[string]any{"tagline": "old", "story": "keep me"}).Save(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Tenant.DeleteOneID(tid).Exec(ctx) })

	// Saved by marketingos after the form was loaded.
	if _, err := marketingos.NewService(db, nil, zap.NewNop()).SaveBrandContext(ctx, tid, marketingos.BrandContext{Positioning: "newer"}); err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]any{
		"name": "Renamed",
		"metadata": map[string]any{
			"tagline":               "new",
			marketingos.MetadataKey: map[string]any{"positioning": "stale"},
		},
	})
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/tenant/profile", bytes.NewReader(body))
	req.Header.Set("X-Tenant-ID", tid.String())
	rec := httptest.NewRecorder()
	NewHandler(db, zap.NewNop()).UpdateProfile(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	got, err := db.Tenant.Get(ctx, tid)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Renamed" || got.Metadata["tagline"] != "new" || got.Metadata["story"] != "keep me" {
		t.Fatalf("profile not saved as sent: name=%q meta=%v", got.Name, got.Metadata)
	}
	bc, _ := got.Metadata[marketingos.MetadataKey].(map[string]any)
	if bc["positioning"] != "newer" {
		t.Fatalf("brand context overwritten by the profile form: %v", bc)
	}
}
