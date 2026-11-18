package creative

import (
	"context"
	"encoding/json"
	"errors"
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

// Each video needs its registry feature; posters need none.
func TestVideoFeature(t *testing.T) {
	cases := []struct {
		kind, video, want string
	}{
		{KindVideo, "", FeatureVideoTemplate},
		{KindVideoAd, "", FeatureVideoTemplate},
		{KindVideoAd, VideoTemplate, FeatureVideoTemplate},
		{KindVideoAd, VideoPresenter, FeatureVideoPresenter},
		{KindVideoAd, VideoPresenterPremium, FeatureVideoPresenterPremium},
		{KindPoster, "", ""},
	}
	for _, c := range cases {
		if got := VideoFeature(c.kind, c.video); got != c.want {
			t.Errorf("VideoFeature(%q, %q) = %q, want %q", c.kind, c.video, got, c.want)
		}
	}
}

// The gate asks the entitlement check, lets gating-exempt people through, and never takes a service
// key's claims as the tenant's exemption.
func TestCheckVideoFeature(t *testing.T) {
	tid := uuid.New()
	var asked []string
	plan := map[string]bool{FeatureVideoTemplate: true}
	s := NewService(nil, zap.NewNop()).WithFeatureCheck(func(_ context.Context, tenantID, feature string) bool {
		if tenantID != tid.String() {
			t.Errorf("asked for tenant %s", tenantID)
		}
		asked = append(asked, feature)
		return plan[feature]
	})
	ctx := context.Background()

	if err := s.CheckVideoFeature(ctx, tid, FeatureVideoTemplate); err != nil {
		t.Fatalf("included feature refused: %v", err)
	}
	err := s.CheckVideoFeature(ctx, tid, FeatureVideoPresenter)
	var nip *NotInPlanError
	if !errors.Is(err, ErrNotInPlan) || !errors.As(err, &nip) || nip.Feature != FeatureVideoPresenter {
		t.Fatalf("missing feature: %v", err)
	}
	if err := s.CheckVideoFeature(ctx, tid, ""); err != nil {
		t.Fatalf("a poster needs no feature: %v", err)
	}

	for name, c := range map[string]*authclient.Claims{
		"platform owner":      {TenantID: tid.String(), IsPlatformOwner: true},
		"demo":                {TenantID: tid.String(), IsDemo: true},
		"pay as you go":       {TenantID: tid.String(), BillingMode: "service_charge"},
		"subscription exempt": {TenantID: tid.String(), SubscriptionExempt: true},
	} {
		asked = nil
		if err := s.CheckVideoFeature(authclient.ContextWithClaims(ctx, c), tid, FeatureVideoPresenter); err != nil {
			t.Errorf("%s refused: %v", name, err)
		}
		if len(asked) != 0 {
			t.Errorf("%s: the entitlement check ran for an exempt person", name)
		}
	}

	// A service key acting for a tenant: its own flags are not the tenant's, so the plan decides.
	svc := &authclient.Claims{IsService: true, ServiceName: "marketflow-ai", IsPlatformOwner: true, TenantID: tid.String()}
	if err := s.CheckVideoFeature(authclient.ContextWithClaims(ctx, svc), tid, FeatureVideoPresenter); !errors.Is(err, ErrNotInPlan) {
		t.Fatalf("service key took the tenant past the plan: %v", err)
	}

	// No check wired: the gate is off.
	if err := NewService(nil, zap.NewNop()).CheckVideoFeature(ctx, tid, FeatureVideoPresenter); err != nil {
		t.Fatalf("gate without a check: %v", err)
	}
}

// Through the handlers: a plan without presenter videos is refused with the plan refusal shape at
// the script, storyboard and render steps; a template video the plan includes goes ahead.
func TestVideoPlanGateHandlers(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	tid := tenantRow(t, db, map[string]any{})
	list := []any{}
	for i := 0; i < 3; i++ {
		list = append(list, map[string]any{"id": uuid.NewString(), "key": brandimages.Prefix(tid) + "2026/11/" + uuid.NewString() + ".jpg"})
	}
	if _, err := db.Tenant.UpdateOneID(tid).SetMetadata(map[string]any{brandimages.MetadataKey: list}).Save(ctx); err != nil {
		t.Fatal(err)
	}
	plan := map[string]bool{FeatureVideoTemplate: true}
	s := NewService(db, zap.NewNop()).WithFeatureCheck(func(_ context.Context, _, feature string) bool { return plan[feature] })
	b := newBrief(t, s, tid)
	h := NewHandler(s, nil, nil, zap.NewNop())
	r := chi.NewRouter()
	r.Post("/creative/briefs/{id}/video-ads", h.CreateVideoAd)
	r.Post("/creative/versions/{id}/render", h.RenderVideoAd)
	r.Post("/creative/versions/{id}/storyboard", h.MakeStoryboard)

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
	refused := func(name string, rec *httptest.ResponseRecorder, feature string) {
		t.Helper()
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body)
		}
		var body struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body.Code != "feature_not_in_plan" || body.Details["feature"] != feature || body.Details["upgrade_path"] != planUpgradePath {
			t.Fatalf("%s: wrong refusal %s", name, rec.Body)
		}
	}

	// The script step refuses a presenter ad the plan does not include.
	refused("presenter script", call("/creative/briefs/"+b.ID.String()+"/video-ads", `{"kind":"presenter"}`, nil), FeatureVideoPresenter)
	refused("premium presenter script", call("/creative/briefs/"+b.ID.String()+"/video-ads", `{"kind":"presenter_premium"}`, nil), FeatureVideoPresenterPremium)

	// A gating-exempt person makes it anyway; then the plan, not the person, decides the later steps.
	rec := call("/creative/briefs/"+b.ID.String()+"/video-ads", `{"kind":"presenter"}`, &authclient.Claims{TenantID: tid.String(), IsDemo: true})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("exempt presenter script: %d %s", rec.Code, rec.Body)
	}
	var made struct {
		ID uuid.UUID `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &made); err != nil || made.ID == uuid.Nil {
		t.Fatalf("no version id: %s", rec.Body)
	}
	// The script is ready for review, so the next steps reach the gate.
	if _, err := db.CreativeVersion.UpdateOneID(made.ID).SetStatus(StatusScriptReady).
		SetCopy(map[string]any{"script": map[string]any{"scenes": []any{}}}).Save(ctx); err != nil {
		t.Fatal(err)
	}
	refused("presenter storyboard", call("/creative/versions/"+made.ID.String()+"/storyboard", ``, nil), FeatureVideoPresenter)
	refused("presenter render", call("/creative/versions/"+made.ID.String()+"/render", ``, nil), FeatureVideoPresenter)

	// A template video the plan includes goes ahead.
	if rec := call("/creative/briefs/"+b.ID.String()+"/video-ads", `{}`, nil); rec.Code != http.StatusAccepted {
		t.Fatalf("template script: %d %s", rec.Code, rec.Body)
	}
}
