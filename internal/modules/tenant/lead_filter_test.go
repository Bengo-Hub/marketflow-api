package tenant

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Item 61: a tenant's own lead filter words are trimmed, lower case and without repeats.
func TestValidateLeadFilterSettingNormalises(t *testing.T) {
	meta := map[string]any{LeadFilterKey: map[string]any{
		"attachment":    []any{"  Industrial   Placement ", "industrial placement", ""},
		"job":           []any{"Graduate Trainee"},
		"service_offer": nil,
	}}
	if err := ValidateLeadFilterSetting(meta); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"attachment": []any{"industrial placement"}, "job": []any{"graduate trainee"}}
	if !reflect.DeepEqual(meta[LeadFilterKey], want) {
		t.Fatalf("got %#v, want %#v", meta[LeadFilterKey], want)
	}
}

func TestValidateLeadFilterSettingClears(t *testing.T) {
	for _, v := range []any{nil, map[string]any{}, map[string]any{"job": []any{"  "}}} {
		meta := map[string]any{LeadFilterKey: v}
		if err := ValidateLeadFilterSetting(meta); err != nil {
			t.Fatalf("%v: %v", v, err)
		}
		if meta[LeadFilterKey] != nil {
			t.Fatalf("%v: want nil, got %#v", v, meta[LeadFilterKey])
		}
	}
	if err := ValidateLeadFilterSetting(map[string]any{"other": 1}); err != nil {
		t.Fatal(err)
	}
}

func TestValidateLeadFilterSettingRefuses(t *testing.T) {
	many := make([]any, maxLeadFilterWords+1)
	for i := range many {
		many[i] = "word " + strings.Repeat("x", i+1)
	}
	for _, v := range []any{
		"internship",
		map[string]any{"spam": []any{"x"}},
		map[string]any{"job": "vacancy"},
		map[string]any{"job": []any{1}},
		map[string]any{"job": []any{strings.Repeat("a", maxLeadFilterWordLen+1)}},
		map[string]any{"job": many},
	} {
		if err := ValidateLeadFilterSetting(map[string]any{LeadFilterKey: v}); err == nil {
			t.Errorf("%v: want an error", v)
		}
	}
}

func TestLeadFilterChanged(t *testing.T) {
	stored := map[string]any{LeadFilterKey: map[string]any{"job": []any{"trainee"}}}
	if LeadFilterChanged(stored, map[string]any{"job": []any{"trainee"}}) {
		t.Error("same words are not a change")
	}
	if !LeadFilterChanged(stored, map[string]any{"job": []any{"trainee", "graduate"}}) {
		t.Error("an added word is a change")
	}
	if LeadFilterChanged(map[string]any{}, nil) {
		t.Error("unset and null are the same")
	}
}

// The KES rate route: KES is 1, another currency reads treasury's rate, no rate is a 503.
func TestGetKESRate(t *testing.T) {
	h := &Handler{logger: zap.NewNop(), KESPer: func(_ context.Context, cur string) (float64, error) {
		if cur == "USD" {
			return 129.5, nil
		}
		return 0, errors.New("no rate")
	}}
	call := func(cur string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/tenant/profile/kes-rate?currency="+cur, nil)
		r.Header.Set("X-Tenant-ID", uuid.NewString())
		w := httptest.NewRecorder()
		h.GetKESRate(w, r)
		return w
	}
	w := call("usd")
	if w.Code != http.StatusOK {
		t.Fatalf("USD: %d %s", w.Code, w.Body)
	}
	var body struct {
		Currency string  `json:"currency"`
		KESPer   float64 `json:"kes_per"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Currency != "USD" || body.KESPer != 129.5 {
		t.Fatalf("USD: %+v", body)
	}
	if w := call("KES"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"kes_per":1`) {
		t.Fatalf("KES: %d %s", w.Code, w.Body)
	}
	if w := call("UGX"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("UGX without a rate: %d", w.Code)
	}
	if w := call("XYZ"); w.Code != http.StatusBadRequest {
		t.Fatalf("XYZ: %d", w.Code)
	}
}
