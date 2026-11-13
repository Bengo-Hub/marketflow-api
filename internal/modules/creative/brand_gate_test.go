package creative

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/modules/brandimages"
)

// Video ads (and their storyboard, render and revision) need the minimum of brand images;
// posters never do, and gating-exempt tenants are not blocked.
func TestVideoAdsNeedBrandImages(t *testing.T) {
	db := openDB(t)
	s := NewService(db, zap.NewNop())
	ctx := context.Background()
	tid := tenantRow(t, db, map[string]any{})
	b := newBrief(t, s, tid)
	h := NewHandler(s, nil, nil, zap.NewNop())
	r := chi.NewRouter()
	r.Post("/creative/briefs/{id}/video-ads", h.CreateVideoAd)
	r.Post("/creative/briefs/{id}/versions", h.CreateVersion)
	r.Post("/creative/versions/{id}/render", h.RenderVideoAd)
	r.Post("/creative/versions/{id}/storyboard", h.MakeStoryboard)
	r.Post("/creative/versions/{id}/revise-video", h.ReviseVideoAd)

	call := func(path, body string, claims *authclient.Claims) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("X-Tenant-ID", tid.String())
		req.Header.Set("Content-Type", "application/json")
		if claims != nil {
			req = req.WithContext(authclient.ContextWithClaims(req.Context(), claims))
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	setImages := func(n int) {
		list := []any{}
		for i := 0; i < n; i++ {
			list = append(list, map[string]any{"id": uuid.NewString(), "key": brandimages.Prefix(tid) + "2026/11/" + uuid.NewString() + ".jpg"})
		}
		// One more under another tenant's prefix never counts.
		list = append(list, map[string]any{"id": "x", "key": brandimages.Prefix(uuid.New()) + "2026/11/x.jpg"})
		if _, err := db.Tenant.UpdateOneID(tid).SetMetadata(map[string]any{brandimages.MetadataKey: list}).Save(ctx); err != nil {
			t.Fatal(err)
		}
	}

	setImages(2)
	rec := call("/creative/briefs/"+b.ID.String()+"/video-ads", `{}`, nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "brand_images_required") ||
		!strings.Contains(rec.Body.String(), "at least 3 brand images") {
		t.Fatalf("video ad with 2 images: %d %s", rec.Code, rec.Body)
	}
	someID := uuid.NewString()
	for _, p := range []string{"/creative/versions/" + someID + "/render", "/creative/versions/" + someID + "/storyboard",
		"/creative/versions/" + someID + "/revise-video"} {
		if rec := call(p, ``, nil); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "brand_images_required") {
			t.Errorf("%s: %d %s", p, rec.Code, rec.Body)
		}
	}
	// Posters are never blocked.
	if rec := call("/creative/briefs/"+b.ID.String()+"/versions", `{"prompt":"jar on linen"}`, nil); rec.Code == http.StatusConflict {
		t.Fatalf("poster blocked: %d %s", rec.Code, rec.Body)
	}
	// A gating-exempt tenant (platform owner, demo, service charge, subscription exempt) is not blocked.
	if rec := call("/creative/briefs/"+b.ID.String()+"/video-ads", `{}`, &authclient.Claims{TenantID: tid.String(), IsDemo: true}); rec.Code != http.StatusAccepted {
		t.Fatalf("exempt tenant: %d %s", rec.Code, rec.Body)
	}
	setImages(3)
	if rec := call("/creative/briefs/"+b.ID.String()+"/video-ads", `{}`, nil); rec.Code != http.StatusAccepted {
		t.Fatalf("video ad with 3 images: %d %s", rec.Code, rec.Body)
	}
	if err := s.WithBrandImagesMin(4).CheckBrandImages(ctx, tid); err == nil {
		t.Fatal("the minimum comes from config")
	}
}
