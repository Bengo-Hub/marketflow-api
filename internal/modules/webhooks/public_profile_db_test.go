package webhooks

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
)

// White label (owner, 2026-11-12): the public profile says whether the business's plan includes
// white_label, so its pages hide "Powered by Codevertex"; no check wired, or a plan without the
// feature, keeps the badge.
func TestPublicProfileWhiteLabel(t *testing.T) {
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", testWebhooksPostgresDSN)
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

	business, lite := uuid.New(), uuid.New()
	for id, slug := range map[uuid.UUID]string{business: "wl-business-" + business.String()[:8], lite: "wl-lite-" + lite.String()[:8]} {
		if _, err := db.Tenant.Create().SetID(id).SetName("WL " + slug).SetSlug(slug).SetStatus("active").Save(ctx); err != nil {
			t.Fatalf("seed tenant: %v", err)
		}
	}
	t.Cleanup(func() {
		for _, id := range []uuid.UUID{business, lite} {
			_ = db.Tenant.DeleteOneID(id).Exec(ctx)
		}
	})

	get := func(h *PublicHandler, id uuid.UUID) bool {
		t.Helper()
		tn, err := db.Tenant.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/public/profile/"+tn.Slug, nil)
		rc := chi.NewRouteContext()
		rc.URLParams.Add("slug", tn.Slug)
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rc))
		rec := httptest.NewRecorder()
		h.GetProfile(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		wl, _ := body["white_label"].(bool)
		return wl
	}

	plain := NewPublicHandler(db, nil, nil, nil, nil, nil, nil, zap.NewNop())
	if get(plain, business) {
		t.Fatal("no feature check wired: the badge stays")
	}
	checked := NewPublicHandler(db, nil, nil, nil, nil, nil, nil, zap.NewNop()).
		WithFeatureCheck(func(_ context.Context, tenantID, feature string) bool {
			return tenantID == business.String() && feature == "white_label"
		})
	if !get(checked, business) {
		t.Fatal("a plan with white_label hides the badge")
	}
	if get(checked, lite) {
		t.Fatal("a plan without white_label keeps the badge")
	}
}
