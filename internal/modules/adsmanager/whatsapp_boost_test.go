package adsmanager

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
)

// A tenant's whatsapp_boost values replace the platform's only when they are valid.
func TestMergeWABoost(t *testing.T) {
	base := WABoostDefaults{BudgetKES: 3000, Days: 7, Objective: "whatsapp"}
	got := mergeWABoost(base, map[string]any{WhatsAppBoostKey: map[string]any{"budget_kes": float64(1500), "days": float64(3), "objective": "traffic",
		"ad_account_connected_id": " acc-1 "}})
	if got != (WABoostDefaults{BudgetKES: 1500, Days: 3, Objective: "traffic", AdAccountConnID: "acc-1"}) {
		t.Fatalf("tenant values: %+v", got)
	}
	bad := mergeWABoost(base, map[string]any{WhatsAppBoostKey: map[string]any{"budget_kes": float64(-5), "days": float64(400), "objective": "leads"}})
	if bad != base {
		t.Fatalf("invalid values must keep the platform's: %+v", bad)
	}
	if mergeWABoost(base, nil) != base || mergeWABoost(base, map[string]any{WhatsAppBoostKey: "x"}) != base {
		t.Fatal("missing or malformed metadata keeps the platform's")
	}
}

// The tenant's chosen Page wins, otherwise the first by name (ad accounts: platform/adaccounts).
func TestPickPage(t *testing.T) {
	a := &ent.ConnectedAccount{ID: uuid.New(), DisplayName: "A"}
	b := &ent.ConnectedAccount{ID: uuid.New(), DisplayName: "B"}
	list := []*ent.ConnectedAccount{a, b}
	if got := pickPage(list, b.ID.String()); got != b {
		t.Fatalf("tenant choice: %s", got.DisplayName)
	}
	if got := pickPage(list, ""); got != a {
		t.Fatalf("pages take the first: %s", got.DisplayName)
	}
	if pickPage(nil, "") != nil {
		t.Fatal("none")
	}
}

// The KES default is converted for another currency, rounded up; no rate means no guess.
func TestBudgetIn(t *testing.T) {
	ctx := context.Background()
	h := &Handler{KESPer: func(_ context.Context, c string) (float64, error) {
		if c == "USD" {
			return 129.5, nil
		}
		return 0, errors.New("no rate")
	}}
	if n, err := h.budgetIn(ctx, 3000, "KES"); err != nil || n != 3000 {
		t.Fatalf("KES: %d %v", n, err)
	}
	if n, err := h.budgetIn(ctx, 3000, "USD"); err != nil || n != 24 {
		t.Fatalf("USD: %d %v", n, err) // 3000 / 129.5 = 23.2, rounded up
	}
	if _, err := h.budgetIn(ctx, 3000, "EUR"); !errors.Is(err, errNoRate) {
		t.Fatalf("no rate: %v", err)
	}
	if _, err := (&Handler{}).budgetIn(ctx, 3000, "USD"); !errors.Is(err, errNoRate) {
		t.Fatalf("no rate source: %v", err)
	}
}

func osBoostRequest(t *testing.T, h *Handler, handler func(*Handler) http.HandlerFunc, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	handler(h)(rec, httptest.NewRequest(http.MethodPost, "/api/v1/internal/ads/os-turn/boost", strings.NewReader(string(b))))
	return rec
}

// The WhatsApp routes refuse before reading anything when the request is incomplete, paid ads are
// off, or the person who tapped may not launch ads; the role check sees that person as the actor.
func TestOSBoostRefusals(t *testing.T) {
	tid, uid := uuid.New(), uuid.New()
	full := map[string]any{"tenant_id": tid.String(), "user_id": uid.String(), "version_id": uuid.NewString(), "budget": 3000, "days": 7}
	quote := func(h *Handler) http.HandlerFunc { return h.OSTurnBoostQuote }
	boost := func(h *Handler) http.HandlerFunc { return h.OSTurnBoost }

	h := &Handler{Logger: zap.NewNop()}
	if rec := osBoostRequest(t, h, boost, map[string]any{"tenant_id": tid.String()}); rec.Code != http.StatusBadRequest {
		t.Fatalf("incomplete: %d", rec.Code)
	}

	off := &Handler{Logger: zap.NewNop(), Enabled: func(context.Context) bool { return false }}
	if rec := osBoostRequest(t, off, quote, full); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "ads_disabled") {
		t.Fatalf("ads off: %d %s", rec.Code, rec.Body)
	}

	var actor uuid.UUID
	deny := &Handler{Logger: zap.NewNop(), CanLaunch: func(ctx context.Context, tenant uuid.UUID) bool {
		actor, _ = actorOf(ctx)
		return false
	}}
	if rec := osBoostRequest(t, deny, boost, full); rec.Code != http.StatusForbidden || actor != uid {
		t.Fatalf("role: %d, actor %s", rec.Code, actor)
	}

	allow := &Handler{Logger: zap.NewNop(), CanLaunch: func(context.Context, uuid.UUID) bool { return true }}
	noQuote := map[string]any{"tenant_id": tid.String(), "user_id": uid.String(), "version_id": uuid.NewString()}
	if rec := osBoostRequest(t, allow, boost, noQuote); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "budget and days") {
		t.Fatalf("a launch needs the quoted budget and days: %d %s", rec.Code, rec.Body)
	}
}
