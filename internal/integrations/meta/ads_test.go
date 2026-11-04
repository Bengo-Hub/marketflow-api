package meta

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// A fake Graph that checks the documented launch shapes: everything PAUSED, lifetime budget with
// an end time, special_ad_categories never missing, the WhatsApp call to action.
func TestAdsLaunchShapes(t *testing.T) {
	forms := map[string]url.Values{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f, _ := url.ParseQuery(string(b))
		forms[r.URL.Path] = f
		switch r.URL.Path {
		case "/v26.0/act_77/adsets":
			_, _ = w.Write([]byte(`{"id":"as1"}`))
		case "/v26.0/act_77/advideos":
			_, _ = w.Write([]byte(`{"id":"vid1"}`))
		case "/v26.0/act_77/adcreatives":
			_, _ = w.Write([]byte(`{"id":"cr1"}`))
		case "/v26.0/act_77/ads":
			_, _ = w.Write([]byte(`{"id":"ad1"}`))
		case "/v26.0/biz1/adaccount":
			_, _ = w.Write([]byte(`{"id":"act_88"}`))
		case "/v26.0/ad1":
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`{"effective_status":"DISAPPROVED","ad_review_feedback":{"global":{"facebook":{"policy":"Misleading claims"}}}}`))
				return
			}
			_, _ = w.Write([]byte(`{"success":true}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	c := NewClient("")
	c.baseURL = srv.URL
	ctx := context.Background()
	start := time.Date(2026, 11, 5, 6, 0, 0, 0, time.UTC)

	if _, err := c.CreateAdSet(ctx, "t", "77", AdSetInput{LifetimeBudget: 500000, Start: start}); err == nil {
		t.Fatal("a lifetime budget without an end must be refused before any call")
	}
	as, err := c.CreateAdSet(ctx, "t", "act_77", AdSetInput{Name: "Holiday", CampaignID: "c1", LifetimeBudget: 500000,
		Start: start, End: start.AddDate(0, 0, 7), OptimizationGoal: "CONVERSATIONS", DestinationType: "WHATSAPP",
		PageID: "pg1", Countries: []string{"KE"}, AgeMin: 18})
	if err != nil || as != "as1" {
		t.Fatalf("ad set: %v", err)
	}
	f := forms["/v26.0/act_77/adsets"]
	if f.Get("status") != "PAUSED" || f.Get("lifetime_budget") != "500000" || f.Get("end_time") == "" || f.Get("destination_type") != "WHATSAPP" {
		t.Fatalf("ad set form: %v", f)
	}
	var tg map[string]any
	_ = json.Unmarshal([]byte(f.Get("targeting")), &tg)
	if tg["age_min"] != float64(18) || !strings.Contains(f.Get("targeting"), `"KE"`) || !strings.Contains(f.Get("promoted_object"), "pg1") {
		t.Fatalf("targeting: %v", f)
	}

	if _, err := c.UploadAdVideo(ctx, "t", "77", "https://media.example/v.mp4", "Holiday"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateAdCreative(ctx, "t", "77", CreativeInput{Name: "x", PageID: "pg1", VideoID: "vid1", CTA: "WHATSAPP_MESSAGE", Message: "Hi"}); err != nil {
		t.Fatal(err)
	}
	if spec := forms["/v26.0/act_77/adcreatives"].Get("object_story_spec"); !strings.Contains(spec, `"app_destination":"WHATSAPP"`) {
		t.Fatalf("creative spec: %s", spec)
	}
	if _, err := c.CreateAd(ctx, "t", "77", "Holiday", "as1", "cr1"); err != nil || forms["/v26.0/act_77/ads"].Get("status") != "PAUSED" {
		t.Fatalf("ad: %v %v", err, forms["/v26.0/act_77/ads"])
	}
	if err := c.SetStatus(ctx, "t", "ad1", "DELETED"); err == nil {
		t.Fatal("only ACTIVE or PAUSED")
	}
	rev, err := c.GetAdReview(ctx, "t", "ad1")
	if err != nil || rev.EffectiveStatus != "DISAPPROVED" || len(rev.Reasons()) != 1 {
		t.Fatalf("review: %+v %v", rev, err)
	}
	if id, err := c.CreateKESAdAccount(ctx, "t", "biz1", "Boi KES", "pg1"); err != nil || id != "act_88" ||
		forms["/v26.0/biz1/adaccount"].Get("timezone_id") != "78" || forms["/v26.0/biz1/adaccount"].Get("currency") != "KES" {
		t.Fatalf("ad account: %v %v", id, forms["/v26.0/biz1/adaccount"])
	}
}

func TestGraphErrorRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Calls to this api have exceeded the rate limit.","code":613}}`))
	}))
	defer srv.Close()
	c := NewClient("")
	c.baseURL = srv.URL
	_, err := c.AdAccounts(context.Background(), "t")
	var ge *GraphError
	if !errors.As(err, &ge) || !ge.RateLimited() {
		t.Fatalf("rate limit not recognised: %v", err)
	}
}

func TestInsightsResults(t *testing.T) {
	var ins Insights
	_ = json.Unmarshal([]byte(`{"actions":[{"action_type":"lead","value":"4"},{"action_type":"onsite_conversion.lead_grouped","value":"4"},
	  {"action_type":"onsite_conversion.messaging_conversation_started_7d","value":"9"},{"action_type":"link_click","value":"40"}]}`), &ins)
	leads, convs := ins.Results()
	if leads != 4 || convs != 9 {
		t.Fatalf("overlapping lead types count once: leads=%d conversations=%d", leads, convs)
	}
}
