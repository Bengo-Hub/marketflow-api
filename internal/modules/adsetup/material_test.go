package adsetup

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Item 68: ad material, manager links and the Meta reads the owner saw fail on 2026-11-25.
func TestMaterialFix(t *testing.T) {
	f := &fake{reads: googleReads(), write: `{}`}
	f.reads["asset_group"] = `{"results":[{"assetGroup":{"id":"44","name":"Main","adStrength":"AVERAGE"}}]}`
	f.reads["asset_group_asset"] = `{"results":[{"assetGroup":{"id":"44"},"assetGroupAsset":{"fieldType":"HEADLINE"},"asset":{"textAsset":{"text":"School fees system"}}}]}`
	in := googleIn(f.server(t))
	in.Profile = func(context.Context) Profile {
		return Profile{Name: "Digitika", Services: []string{"School fees system"}, Videos: []string{"abcdefghijk"}}
	}
	items := googleChecklist(context.Background(), in)
	it := find(t, items, "google.material:44")
	if it.Status != StatusWarning || it.Fix == nil {
		t.Fatalf("%+v", it)
	}
	vals := map[string]string{}
	for _, x := range it.Fix.Fields {
		vals[x.Key] = x.Value
	}
	if vals["headline_1"] != "Digitika" || vals["headline_2"] != "" || vals["video_1"] != "https://youtu.be/abcdefghijk" {
		t.Errorf("proposed %v", vals)
	}
	run := func(v map[string]string) error {
		return runFix(context.Background(), items, fixRequest{Item: it.Key, Token: it.Fix.Token, Input: v})
	}
	for _, bad := range []map[string]string{{}, {"headline_1": "school fees system"}, {"video_1": "https://example.com/abcdefghijk"}} {
		if run(bad) == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	if len(f.writes) != 0 {
		t.Fatal(f.writes)
	}
	if err := run(vals); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"/customers/1/googleAds:mutate", `"text":"Digitika"`, `"youtubeVideoId":"abcdefghijk"`, "assetGroups/44"} {
		if len(f.writes) != 1 || !strings.Contains(f.writes[0], s) {
			t.Errorf("no %s in %v", s, f.writes)
		}
	}
}

func TestManagerLinkFixes(t *testing.T) {
	f := &fake{reads: googleReads(), write: `{"results":[{"resourceName":"x"}]}`}
	f.reads["customer_manager_link"] = `{"results":[{"customerManagerLink":{"resourceName":"customers/1/customerManagerLinks/9320849278~5","managerCustomer":"customers/9320849278","status":"PENDING"}}]}`
	in := googleIn(f.server(t))
	run := func() {
		t.Helper()
		items := googleChecklist(context.Background(), in)
		it := find(t, items, "google.manager")
		if it.Fix == nil {
			t.Fatalf("no fix: %+v", it)
		}
		if err := runFix(context.Background(), items, fixRequest{Item: it.Key, Token: it.Fix.Token}); err != nil {
			t.Fatal(err)
		}
	}
	run()
	f.reads["customer_manager_link"] = `{}`
	if it := find(t, googleChecklist(context.Background(), in), "google.manager"); it.Fix != nil || len(it.Steps) == 0 {
		t.Errorf("a person without the manager account gets steps: %+v", it)
	}
	f.reads["/v26/customers:listAccessibleCustomers"] = `{"resourceNames":["customers/9320849278"]}`
	run()
	if len(f.writes) != 2 || !strings.Contains(f.writes[0], "/customers/1/customerManagerLinks:mutate") || !strings.Contains(f.writes[0], `"ACTIVE"`) ||
		!strings.Contains(f.writes[1], "/customers/9320849278/customerClientLinks:mutate") {
		t.Errorf("%v", f.writes)
	}
}

func TestMetaDomainAndPrivacyReads(t *testing.T) {
	site := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/privacy" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `<a class="x" href="/privacy">Privacy</a>`)
	}))
	defer site.Close()
	hc := site.Client()
	ctx := context.Background()
	if !opens(ctx, hc, site.URL+"/privacy") || opens(ctx, hc, site.URL+"/privay") {
		t.Error("the link is opened before Meta gets it")
	}
	if b := pageBody(ctx, hc, site.URL); !strings.Contains(b, "/privacy") {
		t.Error(b)
	}
	in := metaInput{Website: "www.shop.example.com", TXT: func(_ context.Context, h string) ([]string, error) {
		if h == "example.com" {
			return []string{"v=spf1", "facebook-domain-verification=abc"}, nil
		}
		return nil, nil
	}}
	if !in.domainProven(ctx) {
		t.Error("the parent domain's code counts")
	}
	in.TXT = nil
	if in.domainProven(ctx) {
		t.Error("no code, not proven")
	}
	for _, c := range []struct {
		biz    string
		proven bool
		want   string
	}{{"", true, StatusInfo}, {"b", true, StatusInfo}, {"b", false, StatusWarning}} {
		if it := in.domainItem(c.biz, c.proven, nil); it.Status != c.want {
			t.Errorf("%+v: %s", c, it.Status)
		}
	}
}

func TestUnreadKeepsTheProvidersWordsApart(t *testing.T) {
	it := seal([]Item{unread("google.account", "x", "Google", io.ErrUnexpectedEOF, nil)})[0]
	if strings.Contains(it.Detail, "EOF") || it.Support == "" || len(it.Steps) == 0 {
		t.Errorf("%+v", it)
	}
}
