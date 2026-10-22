package tiktok

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// fakeTikTok records the last request and answers with body (HTTP 200, as TikTok does for API
// errors too: the error is the envelope's code).
type fakeTikTok struct {
	path, token string
	query       url.Values
	payload     map[string]any
	body        string
}

func (f *fakeTikTok) client(t *testing.T) *Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.path, f.token, f.query = r.URL.Path, r.Header.Get("Access-Token"), r.URL.Query()
		f.payload = nil
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			_ = json.Unmarshal(b, &f.payload)
		}
		_, _ = io.WriteString(w, f.body)
	}))
	t.Cleanup(srv.Close)
	c := NewClient()
	c.baseURL = srv.URL
	return c
}

func TestCreateAndUpdateCampaign(t *testing.T) {
	f := &fakeTikTok{body: `{"code":0,"message":"OK","data":{"campaign_id":"1800"}}`}
	c := f.client(t)
	id, err := c.CreateCampaign(context.Background(), "adv1", "tok", CampaignInput{
		CampaignName: "Spring", ObjectiveType: "LEAD_GENERATION", BudgetMode: "BUDGET_MODE_DAY", Budget: 50,
	})
	if err != nil || id != "1800" {
		t.Fatalf("create: %q %v", id, err)
	}
	if f.path != "/campaign/create/" || f.token != "tok" || f.payload["advertiser_id"] != "adv1" || f.payload["budget"] != float64(50) {
		t.Errorf("request: %s %s %v", f.path, f.token, f.payload)
	}

	if err := c.UpdateCampaignStatus(context.Background(), "adv1", "tok", "1800", "DISABLE"); err != nil {
		t.Fatal(err)
	}
	if f.path != "/campaign/update/" || f.payload["operation_status"] != "DISABLE" || f.payload["campaign_id"] != "1800" {
		t.Errorf("update: %s %v", f.path, f.payload)
	}

	f.body = `{"code":40002,"message":"Insufficient permissions"}`
	if _, err := c.CreateCampaign(context.Background(), "adv1", "tok", CampaignInput{}); err == nil || !strings.Contains(err.Error(), "40002") {
		t.Errorf("a non-zero code is an error: %v", err)
	}
	if err := c.UpdateCampaignStatus(context.Background(), "adv1", "tok", "1800", "ENABLE"); err == nil {
		t.Error("a failed update is an error")
	}
	f.body = "<html>gateway error</html>"
	if _, err := c.CreateCampaign(context.Background(), "adv1", "tok", CampaignInput{}); err == nil {
		t.Error("a non-JSON answer is an error")
	}
}

func TestGetCampaignReport(t *testing.T) {
	f := &fakeTikTok{body: `{"code":0,"data":{"list":[{"metrics":{"spend":12.5,"impressions":1000,"clicks":40,"leads":3}}]}}`}
	c := f.client(t)
	m, err := c.GetCampaignReport(context.Background(), "adv1", "tok", "1800")
	if err != nil || m.Spend != 12.5 || m.Impressions != 1000 || m.Leads != 3 {
		t.Fatalf("report: %+v %v", m, err)
	}
	if f.path != "/report/integrated/get/" || f.query.Get("advertiser_id") != "adv1" {
		t.Errorf("request: %s %v", f.path, f.query)
	}
	// The campaign filter is a JSON array inside a JSON string, as the reporting API expects.
	var filters []map[string]string
	if err := json.Unmarshal([]byte(f.query.Get("filters")), &filters); err != nil || len(filters) != 1 || filters[0]["filter_value"] != `["1800"]` {
		t.Errorf("filters: %q %v", f.query.Get("filters"), err)
	}
	f.body = `{"code":0,"data":{"list":[]}}`
	if m, err := c.GetCampaignReport(context.Background(), "adv1", "tok", "1800"); err != nil || m != (Metrics{}) {
		t.Errorf("no rows gives zero metrics: %+v %v", m, err)
	}
}
