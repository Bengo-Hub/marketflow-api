package adsetup

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bengobox/marketflow-api/internal/integrations/google"
	"github.com/bengobox/marketflow-api/internal/integrations/meta"
	"github.com/bengobox/marketflow-api/internal/integrations/tiktok"
)

// fake records writes and answers reads by the first matching key.
type fake struct {
	mu     sync.Mutex
	reads  map[string]string // substring of the query or path to the body
	writes []string          // path + " " + body
	write  string
}

func (f *fake) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		key := r.URL.Path
		if strings.HasSuffix(key, "googleAds:search") {
			var q struct{ Query string }
			_ = json.Unmarshal(b, &q)
			_, rest, _ := strings.Cut(q.Query, " FROM ")
			key = strings.Fields(rest)[0]
		} else if r.Method == http.MethodPost {
			f.mu.Lock()
			f.writes = append(f.writes, r.URL.Path+" "+string(b))
			f.mu.Unlock()
			_, _ = io.WriteString(w, f.write)
			return
		} else if fl := r.URL.Query().Get("fields"); fl != "" && f.reads[key+"?"+fl] != "" {
			key += "?" + fl
		}
		body, ok := f.reads[key]
		if !ok {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"message":"not allowed","code":200},"code":40001,"message":"not allowed"}`)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func find(t *testing.T, items []Item, key string) Item {
	t.Helper()
	for _, i := range items {
		if i.Key == key {
			return i
		}
	}
	t.Fatalf("no item %s", key)
	return Item{}
}

func want(t *testing.T, items []Item, status map[string]string) {
	t.Helper()
	for k, s := range status {
		if got := find(t, items, k); got.Status != s {
			t.Errorf("%s: %s (%s), want %s", k, got.Status, got.Detail, s)
		}
	}
}

func googleReads() map[string]string {
	return map[string]string{
		"customer":                        `{"results":[{"customer":{"status":"ENABLED","currencyCode":"KES"}}]}`,
		"billing_setup":                   `{"results":[{"billingSetup":{"status":"APPROVED"}}]}`,
		"customer_manager_link":           `{"results":[{"customerManagerLink":{"managerCustomer":"customers/9320849278","status":"PENDING"}}]}`,
		"conversion_action":               `{}`,
		"campaign":                        `{"results":[{"campaign":{"id":"7","resourceName":"customers/1/campaigns/7","name":"Own goals","primaryStatus":"LIMITED","primaryStatusReasons":["BUDGET_CONSTRAINED"]}},{"campaign":{"id":"8","resourceName":"customers/1/campaigns/8","name":"Defaults","primaryStatus":"ELIGIBLE"}}]}`,
		"customer_conversion_goal":        `{}`,
		"campaign_conversion_goal":        `{}`,
		"conversion_goal_campaign_config": `{"results":[{"conversionGoalCampaignConfig":{"campaign":"customers/1/campaigns/7","goalConfigLevel":"CAMPAIGN"}}]}`,
		"customer_asset":                  `{}`,
		"campaign_asset":                  `{"results":[{"campaign":{"id":"7"},"campaignAsset":{"fieldType":"CALL"}}]}`,
		"user_list":                       `{}`,
		"ad_group_ad":                     `{}`,
	}
}

func googleIn(srv *httptest.Server) googleInput {
	return googleInput{Ads: google.Ads{HTTP: srv.Client(), Base: srv.URL, Version: "v26", AccessToken: "tok"}, CustomerID: "1",
		ManagerID: "9320849278", BillingURL: "https://ads.google.com/aw/billing/summary", Cfg: defaults(),
		Consented: func(context.Context) (int, error) { return 12, nil },
		Profile: func(context.Context) Profile {
			return Profile{Country: "KE", Phone: "254743793901", City: "Kisumu", Services: []string{"School fees system", strings.Repeat("x", 40)},
				Pages: []Page{{"Pricing", "https://example.com/pricing"}}}
		}}
}

func TestGoogleNewAccountAndFixes(t *testing.T) {
	f := &fake{reads: googleReads(), write: `{"results":[{"resourceName":"customers/1/conversionActions/5"}]}`}
	in := googleIn(f.server(t))
	items := googleChecklist(context.Background(), in)
	want(t, items, map[string]string{
		"google.account": StatusOK, "google.manager": StatusWarning, "google.conversion_action": StatusAction, "google.mapping": StatusAction,
		"google.goals": StatusAction, "google.auto_tagging": StatusAction, "google.status": StatusWarning, "google.sitelinks": StatusWarning,
		"google.call": StatusWarning, "google.callouts": StatusWarning, "google.customer_list": StatusInfo,
	})
	if len(f.writes) != 0 {
		t.Fatalf("reading the checklist wrote: %v", f.writes)
	}
	if find(t, items, "google.mapping").Fix != nil || find(t, items, "google.customer_list").Fix != nil {
		t.Error("no mapping fix before the conversion exists, and never a customer list fix")
	}
	run := func(key string, v map[string]string) error {
		it := find(t, items, key)
		return runFix(context.Background(), items, fixRequest{Item: key, Token: it.Fix.Token, Input: v})
	}
	if err := run("google.conversion_action", nil); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"/v26/customers/1/conversionActions:mutate", `"UPLOAD_CLICKS"`, `"ONE_PER_CLICK"`, `"clickThroughLookbackWindowDays":"90"`, `"QUALIFIED_LEAD"`, `"defaultCurrencyCode":"KES"`} {
		if !strings.Contains(f.writes[0], s) {
			t.Errorf("conversion action lacks %s: %s", s, f.writes[0])
		}
	}
	if err := run("google.auto_tagging", nil); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(f.writes[1], "/v26/customers/1:mutate ") || !strings.Contains(f.writes[1], `"autoTaggingEnabled":true`) {
		t.Errorf("auto-tagging: %s", f.writes[1])
	}
	// The owner's reviewed values are used, and only those.
	if err := run("google.sitelinks", map[string]string{"text_1": "Prices", "url_1": "http://example.com"}); err == nil {
		t.Error("a plain http sitelink must be refused")
	}
	if err := run("google.sitelinks", map[string]string{}); err == nil {
		t.Error("an empty form must create nothing")
	}
	if len(f.writes) != 2 {
		t.Fatalf("refused input reached Google: %v", f.writes[2:])
	}
	if err := run("google.call", map[string]string{"country": "ke", "phone": "0743793901"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.writes[2], "/assets:mutate") || !strings.Contains(f.writes[2], `"countryCode":"KE"`) || !strings.Contains(f.writes[3], "/customerAssets:mutate") {
		t.Errorf("call asset: %v", f.writes[2:])
	}
	if co := find(t, items, "google.callouts").Fix.Fields; co[0].Value != "Based in Kisumu" || co[1].Value != "School fees system" || co[2].Value != "" {
		t.Errorf("callouts must come from the profile and fit: %+v", co)
	}
}

func TestGoogleGoals(t *testing.T) {
	f := &fake{reads: googleReads(), write: `{"results":[]}`}
	f.reads["conversion_action"] = `{"results":[{"conversionAction":{"resourceName":"customers/1/conversionActions/5","name":"Qualified lead (MarketFlow)","type":"UPLOAD_CLICKS","category":"QUALIFIED_LEAD","origin":"WEBSITE","primaryForGoal":true}}]}`
	in := googleIn(f.server(t))
	var saved map[string]string
	in.Mapping = map[string]string{"Purchase": "customers/1/conversionActions/9"}
	in.SaveMapping = func(_ context.Context, m map[string]string) error { saved = m; return nil }
	items := googleChecklist(context.Background(), in)
	want(t, items, map[string]string{"google.conversion_action": StatusOK, "google.mapping": StatusAction, "google.goals": StatusAction})
	for _, k := range []string{"google.mapping", "google.goals"} {
		if err := runFix(context.Background(), items, fixRequest{Item: k, Token: find(t, items, k).Fix.Token}); err != nil {
			t.Fatal(err)
		}
	}
	if saved["Lead"] != "customers/1/conversionActions/5" || saved["Purchase"] == "" {
		t.Errorf("the other events' mapping must stay: %v", saved)
	}
	if len(f.writes) != 2 || !strings.Contains(f.writes[0], "customerConversionGoals/QUALIFIED_LEAD~WEBSITE") ||
		!strings.Contains(f.writes[1], "campaignConversionGoals/7~QUALIFIED_LEAD~WEBSITE") || strings.Contains(f.writes[1], "/8~") {
		t.Errorf("goals: %v", f.writes)
	}

	f.reads["campaign_conversion_goal"] = `{"results":[{"campaignConversionGoal":{"campaign":"customers/1/campaigns/7","category":"QUALIFIED_LEAD","origin":"WEBSITE","biddable":true}},{"campaignConversionGoal":{"campaign":"customers/1/campaigns/8","category":"QUALIFIED_LEAD","origin":"WEBSITE","biddable":true}}]}`
	want(t, googleChecklist(context.Background(), in), map[string]string{"google.goals": StatusOK})
}

// One refused read hides one check; an empty billing read is not proof of no billing.
func TestGoogleRefusedReads(t *testing.T) {
	f := &fake{reads: googleReads()}
	delete(f.reads, "customer_manager_link")
	f.reads["billing_setup"] = `{}`
	want(t, googleChecklist(context.Background(), googleIn(f.server(t))), map[string]string{
		"google.manager": StatusUnknown, "google.account": StatusWarning, "google.auto_tagging": StatusAction})
	delete(f.reads, "customer")
	if items := googleChecklist(context.Background(), googleIn(f.server(t))); len(items) != 1 || items[0].Status != StatusUnknown {
		t.Errorf("an unreadable account gives one item: %+v", items)
	}
}

func TestRunFixRefusals(t *testing.T) {
	ran := false
	items := seal([]Item{{Key: "a", Fix: &Fix{Changes: []string{"one"}, run: func(context.Context, map[string]string) error { ran = true; return nil }}}, {Key: "b"}})
	for _, in := range []fixRequest{{Item: "a", Token: "old"}, {Item: "a"}, {Item: "b"}, {Item: "c"}} {
		var stop *errStop
		if err := runFix(context.Background(), items, in); !errors.As(err, &stop) {
			t.Errorf("%+v: %v", in, err)
		}
	}
	if ran {
		t.Error("a refused fix ran")
	}
}

func metaIn(srv *httptest.Server) metaInput {
	return metaInput{Client: meta.NewClient("").WithBase(srv.URL, srv.Client()), Token: "SECRET", ActID: "act_77", Business: "Digitika",
		Pages: []metaPage{{"p1", "Digitika", "ptok"}}, Website: "www.digitika.example", Now: time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC), Cfg: defaults()}
}

const actFields = "id,account_id,name,account_status,disable_reason,currency,business_country_code,timezone_name,amount_spent,spend_cap,balance,is_prepay_account,funding_source,funding_source_details,min_daily_budget"

func metaReads() map[string]string {
	return map[string]string{
		"/v27.0/act_77?" + actFields:           `{"account_status":1,"currency":"KES","spend_cap":"100000","amount_spent":"25000","funding_source":"9"}`,
		"/v27.0/act_77/adspixels":              `{"data":[]}`,
		"/v27.0/act_77/promote_pages":          `{"data":[{"id":"p1"}]}`,
		"/v27.0/act_77?business":               `{"business":{"id":"b1"}}`,
		"/v27.0/b1/owned_domains":              `{"data":[{"domain_name":"other.example"}]}`,
		"/v27.0/act_77/ads":                    `{"data":[{"name":"Ad","effective_status":"DISAPPROVED","issues_info":[{"error_summary":"Misleading"}]}]}`,
		"/v27.0/p1/leadgen_forms":              `{"data":[{"id":"1","name":"Old","status":"ARCHIVED"}]}`,
		"/v27.0/p1?leadgen_tos_accepted":       `{"leadgen_tos_accepted":true}`,
		"/v27.0/p1?instagram_business_account": `{"id":"p1"}`,
		"/v27.0/p1?whatsapp_number,has_whatsapp_number,has_whatsapp_business_number": `{"id":"p1"}`,
	}
}

func TestMetaChecklistAndFixes(t *testing.T) {
	f := &fake{reads: metaReads(), write: `{"id":"px1"}`}
	in := metaIn(f.server(t))
	items := metaChecklist(context.Background(), in)
	want(t, items, map[string]string{
		"meta.account": StatusOK, "meta.funding": StatusOK, "meta.spend_cap": StatusOK, "meta.pixel": StatusAction, "meta.conversions_api": StatusWarning,
		"meta.page": StatusOK, "meta.lead_form:p1": StatusWarning, "meta.instagram:p1": StatusWarning,
		// Fields Meta leaves out are unknown, not "no number".
		"meta.whatsapp:p1": StatusUnknown, "meta.domain": StatusWarning, "meta.policy": StatusAction,
	})
	if d := find(t, items, "meta.spend_cap").Detail; !strings.Contains(d, "KES 1000.00") || !strings.Contains(d, "KES 750.00 left") {
		t.Error(d)
	}
	if len(f.writes) != 0 {
		t.Fatalf("reading wrote: %v", f.writes)
	}
	run := func(key string, v map[string]string) error {
		return runFix(context.Background(), items, fixRequest{Item: key, Token: find(t, items, key).Fix.Token, Input: v})
	}
	if err := run("meta.pixel", map[string]string{"name": "Digitika pixel"}); err != nil || !strings.HasPrefix(f.writes[0], "/v27.0/act_77/adspixels ") {
		t.Errorf("pixel: %v %v", err, f.writes)
	}
	if err := run("meta.lead_form:p1", map[string]string{"name": "Leads"}); err == nil {
		t.Error("a form without a privacy policy must be refused")
	}
	if err := run("meta.lead_form:p1", map[string]string{"name": "Leads", "privacy_url": "https://digitika.example/privacy"}); err != nil ||
		!strings.HasPrefix(f.writes[1], "/v27.0/p1/leadgen_forms ") || !strings.Contains(f.writes[1], "access_token=ptok") {
		t.Errorf("form: %v %v", err, f.writes)
	}
	// Without the permission the fix is not offered.
	in.Scopes = []string{"ads_read"}
	items = metaChecklist(context.Background(), in)
	if find(t, items, "meta.pixel").Fix != nil || find(t, items, "meta.lead_form:p1").Fix != nil {
		t.Error("a fix was offered without the permission")
	}
}

func TestMetaConversionsAPI(t *testing.T) {
	f := &fake{reads: metaReads()}
	f.reads["/v27.0/act_77/adspixels"] = `{"data":[{"id":"55","name":"P","last_fired_time":"2026-11-24T10:00:00+0000"}]}`
	f.reads["/v27.0/dataset_quality"] = `{"web":[{"event_name":"Lead","event_match_quality":{"composite_score":4.2}}]}`
	in := metaIn(f.server(t))
	sent := ""
	in.StartSending = func(_ context.Context, id string) error { sent = id; return nil }
	items := metaChecklist(context.Background(), in)
	want(t, items, map[string]string{"meta.pixel": StatusOK, "meta.conversions_api": StatusWarning})
	if err := runFix(context.Background(), items, fixRequest{Item: "meta.conversions_api", Token: find(t, items, "meta.conversions_api").Fix.Token}); err != nil || sent != "55" {
		t.Errorf("%v %q", err, sent)
	}
	in.SendingTo = "55"
	it := find(t, metaChecklist(context.Background(), in), "meta.conversions_api")
	if it.Status != StatusWarning || it.Fix != nil || len(it.Points) != 1 {
		t.Errorf("a low match score is a warning: %+v", it)
	}
	delete(f.reads, "/v27.0/dataset_quality")
	want(t, metaChecklist(context.Background(), in), map[string]string{"meta.conversions_api": StatusUnknown})
}

// A network error repeats the request address, which holds the access token.
func TestNoTokenInErrors(t *testing.T) {
	f := &fake{}
	srv := f.server(t)
	in := metaIn(srv)
	srv.Close()
	b, _ := json.Marshal(metaChecklist(context.Background(), in))
	if strings.Contains(string(b), "SECRET") || strings.Contains(string(b), "ptok") {
		t.Errorf("a token reached the checklist: %s", b)
	}
	if s := safeErr(&url.Error{Op: "Get", URL: "https://x/?access_token=SECRET", Err: errors.New("eof")}); strings.Contains(s, "SECRET") {
		t.Error(s)
	}
}

func TestTikTokChecklist(t *testing.T) {
	f := &fake{write: `{"code":0,"data":{"pixel_id":"1"}}`, reads: map[string]string{
		"/advertiser/info/": `{"code":0,"data":{"list":[{"name":"A","status":"STATUS_ENABLE","currency":"KES","balance":0,"owner_bc_id":"b"}]}}`,
		"/pixel/list/":      `{"code":0,"data":{"pixels":[]}}`,
		"/ad/get/":          `{"code":0,"data":{"list":[{"ad_name":"One","secondary_status":"AD_STATUS_AUDIT_DENY"}]}}`,
	}}
	srv := f.server(t)
	in := tiktokInput{Client: tiktok.NewClient().WithBase(srv.URL, srv.Client()), Token: "t", AdvertiserID: "9", Business: "Boi", Cfg: defaults(),
		HomeURL: "https://ads.tiktok.com/", PaymentURL: "https://ads.tiktok.com/pay"}
	items := tiktokChecklist(context.Background(), in)
	want(t, items, map[string]string{"tiktok.account": StatusOK, "tiktok.funding": StatusWarning, "tiktok.pixel": StatusWarning, "tiktok.events": StatusWarning,
		"tiktok.lead_form": StatusUnknown, "tiktok.business_center": StatusOK, "tiktok.policy": StatusAction})
	it := find(t, items, "tiktok.pixel")
	if err := runFix(context.Background(), items, fixRequest{Item: it.Key, Token: it.Fix.Token, Input: map[string]string{"name": "Boi pixel"}}); err != nil ||
		len(f.writes) != 1 || !strings.Contains(f.writes[0], `"advertiser_id":"9"`) {
		t.Errorf("%v %v", err, f.writes)
	}
	f.write = `{"code":40001,"message":"No permission"}`
	var ae *tiktok.APIError
	if err := runFix(context.Background(), items, fixRequest{Item: it.Key, Token: it.Fix.Token, Input: map[string]string{"name": "x"}}); !errors.As(err, &ae) {
		t.Errorf("TikTok's refusal must come back: %v", err)
	}
}

func TestConfigAndPages(t *testing.T) {
	c := parseConfig(func(k string) string {
		return map[string]string{KeyListMin: "100", KeyActionWindow: "400", KeyLinks: `{"meta_ads":"javascript:alert(1)","google_goals":"https://ads.google.com/x"}`}[k]
	})
	if c.ListMin != 100 || c.WindowDays != 90 || !strings.HasPrefix(c.Links["meta_ads"], "https://") || c.Links["google_goals"] != "https://ads.google.com/x" {
		t.Errorf("%+v", c)
	}
	if l := c.link("meta_billing", "x", "act_7"); !strings.HasSuffix(l.URL, "asset_id=7") {
		t.Error(l.URL)
	}
	got := distinctPages(linksIn("https://www.boi.example/", `<a class="x" href="/shop?x=1">Our <b>shop</b></a><a href="/shop/">Shop</a>
		<a href="https://evil.example/a">Other</a><a href="/login">Log in</a><a href="/">Home</a><a href="javascript:x()">J</a><a href='/about'>About&amp;us</a>`))
	if len(got) != 2 || got[0] != (Page{"Our shop", "https://www.boi.example/shop"}) || got[1].Title != "About&us" {
		t.Errorf("%+v", got)
	}
}
