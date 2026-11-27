package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent/providersetting"
	"github.com/bengobox/marketflow-api/internal/platform/modelcatalog"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

var testFal = map[string]string{
	"fal-ai/nano-banana-pro-2":                   "text-to-image",
	"fal-ai/kling-video/v3.5/pro/image-to-video": "image-to-video",
	"bytedance/seedance-2.5/us/text-to-video":    "text-to-video",
}

// A patch changes only the keys it names: nothing else is written or deleted. A change keeps the
// value it replaced for revert; an empty value resets the tier.
func TestPlanRoutePatchMerges(t *testing.T) {
	routes := map[string]string{
		"media.route.video_standard":              "fal-ai/minimax/hailuo-2.4-fast/standard/image-to-video",
		"media.route.voice_fal_standard":          "elevenlabs/tts/eleven-v5-turbo",
		"media.route.voice_fal_standard.previous": "elevenlabs/tts/eleven-v4-turbo",
	}
	set, del, err := planRoutePatch(map[string]string{
		"media.route.poster_premium":     "fal-ai/nano-banana-pro-2",
		"media.route.voice_fal_standard": "",
		"media.route.voice_standard":     "eleven_v5_turbo",
	}, routes, map[string]string{"voice_model": "eleven_v4_turbo"}, testFal)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"media.route.poster_premium":          "fal-ai/nano-banana-pro-2",
		"media.route.poster_premium.previous": "fal-ai/nano-banana-pro", // the default it replaced
		"media.route.voice_standard":          "eleven_v5_turbo",
		"media.route.voice_standard.previous": "eleven_v4_turbo", // the legacy card setting
	}
	if len(set) != len(want) {
		t.Fatalf("set %v", set)
	}
	for k, v := range want {
		if set[k] != v {
			t.Errorf("%s = %q, want %q", k, set[k], v)
		}
	}
	if strings.Join(del, ",") != "media.route.voice_fal_standard,media.route.voice_fal_standard.previous" {
		t.Fatalf("del %v", del)
	}
	if _, ok := set["media.route.video_standard"]; ok {
		t.Fatal("a key not in the patch was written")
	}

	// Saving what already runs writes nothing.
	if set, del, err := planRoutePatch(map[string]string{"media.route.video_standard": routes["media.route.video_standard"]}, routes, nil, nil); err != nil || len(set)+len(del) != 0 {
		t.Fatalf("no-op save: %v %v %v", set, del, err)
	}
	// A flag keeps no previous value.
	if set, _, err := planRoutePatch(map[string]string{"media.route.voice_timings_via_scribe": "true"}, nil, nil, nil); err != nil || len(set) != 1 {
		t.Fatalf("flag: %v %v", set, err)
	}
}

func TestPlanRoutePatchRefuses(t *testing.T) {
	for name, patch := range map[string]map[string]string{
		"unknown key":         {"media.route.poster_ultra": "fal-ai/x"},
		"server-kept key":     {"media.route.poster_premium.previous": "fal-ai/x"},
		"not in fal's list":   {"media.route.poster_premium": "fal-ai/nano-banana-pro-9"},
		"wrong category":      {"media.route.video_premium": "bytedance/seedance-2.5/us/text-to-video"},
		"not an endpoint":     {"media.route.poster_standard": "https://example.com/model"},
		"not an eleven model": {"media.route.voice_premium": "Eleven v5"},
		"flag":                {"media.route.voice_timings_via_scribe": "maybe"},
	} {
		if _, _, err := planRoutePatch(patch, nil, nil, testFal); !errors.Is(err, errRouteValue) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Without fal's list the format is checked and the rest is left to marketflow-ai.
	if _, _, err := planRoutePatch(map[string]string{"media.route.poster_premium": "fal-ai/nano-banana-pro-9"}, nil, nil, nil); err != nil {
		t.Fatalf("format-only check: %v", err)
	}
}

// fakeAlerter records alerts.
type fakeAlerter struct{ got chan string }

func (a fakeAlerter) Send(_ context.Context, subject, text, kind, key string) {
	a.got <- subject + "\n" + text
}

// Against the database: a patch keeps the other keys, and Sync models upgrades a tier within its
// family, keeps the replaced model for revert and tells the admins.
func TestMediaRoutesStore(t *testing.T) {
	db, _ := openCostReportDB(t)
	ctx := context.Background()
	env := "test-" + uuid.NewString()[:8]
	t.Cleanup(func() {
		_, _ = db.ProviderSetting.Delete().Where(providersetting.EnvironmentEQ(env)).Exec(ctx)
	})
	alerts := fakeAlerter{got: make(chan string, 1)}
	h := (&PlatformProviderHandler{client: db, logger: zap.NewNop(), keys: encryption.NewKeyProvider(nil, "")}).WithAlerts(alerts)

	if err := h.writeRouteSettings(ctx, env, map[string]string{"media.route.presenter": "veed/fabric-2.0", "media.route.voice_timings_via_scribe": "true"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.writeRouteSettings(ctx, env, map[string]string{"media.route.presenter": "veed/fabric-2.1"}, nil); err != nil {
		t.Fatal(err)
	}
	got, err := h.routeSettings(ctx, env)
	if err != nil || got["media.route.presenter"] != "veed/fabric-2.1" || got["media.route.voice_timings_via_scribe"] != "true" || len(got) != 2 {
		t.Fatalf("after two writes: %v %v", got, err)
	}

	listed := []modelcatalog.Model{
		{ID: "fal-ai/kling-video/v3/pro/image-to-video", Category: "image-to-video"},
		{ID: "fal-ai/kling-video/v3.5/pro/image-to-video", Category: "image-to-video"},
		{ID: "elevenlabs/tts/eleven-v5-preview", Category: "text-to-speech"},
	}
	sw := h.autoPickRoutes(ctx, "fal", env, listed)
	if len(sw) != 1 || sw[0].To != "fal-ai/kling-video/v3.5/pro/image-to-video" || sw[0].From != "fal-ai/kling-video/v3/pro/image-to-video" {
		t.Fatalf("switches %+v", sw)
	}
	got, _ = h.routeSettings(ctx, env)
	if got["media.route.video_premium"] != sw[0].To || got["media.route.video_premium.previous"] != sw[0].From ||
		got["media.route.presenter"] != "veed/fabric-2.1" || got["media.route.voice_timings_via_scribe"] != "true" {
		t.Fatalf("after the upgrade: %v", got)
	}
	select {
	case msg := <-alerts.got:
		if !strings.Contains(msg, sw[0].To) {
			t.Fatalf("alert: %s", msg)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no alert for the upgrade")
	}
	if again := h.autoPickRoutes(ctx, "fal", env, listed); len(again) != 0 {
		t.Fatalf("a second sync switches nothing: %+v", again)
	}
}

func TestFetchFalPrices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Key k" || len(r.URL.Query()["endpoint_id"]) != 2 {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"prices": []map[string]any{
			{"endpoint_id": "elevenlabs/tts/eleven-v4", "unit_price": 0.08, "unit": "1000 characters", "currency": "USD"},
			{"endpoint_id": "fal-ai/elevenlabs/speech-to-text/scribe-v2", "unit_price": 0.008, "unit": "minutes", "currency": "USD"},
		}})
	}))
	t.Cleanup(srv.Close)
	prev := falPricingBase
	falPricingBase = srv.URL
	t.Cleanup(func() { falPricingBase = prev })
	got, err := fetchFalPrices(context.Background(), "k", []string{"elevenlabs/tts/eleven-v4", "fal-ai/elevenlabs/speech-to-text/scribe-v2"})
	if err != nil || got["elevenlabs/tts/eleven-v4"].UnitPrice != 0.08 || got["fal-ai/elevenlabs/speech-to-text/scribe-v2"].Unit != "minutes" {
		t.Fatalf("%v %+v", err, got)
	}
}
