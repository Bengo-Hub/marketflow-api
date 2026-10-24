package meta

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// fakeGraph records the last request and answers with status and body.
type fakeGraph struct {
	method, path string
	query, form  url.Values
	status       int
	body         string
}

func (f *fakeGraph) client(t *testing.T) *Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.method, f.path, f.query = r.Method, r.URL.EscapedPath(), r.URL.Query()
		b, _ := io.ReadAll(r.Body)
		f.form, _ = url.ParseQuery(string(b))
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, f.body)
	}))
	t.Cleanup(srv.Close)
	c := NewClient("")
	c.baseURL = srv.URL
	return c
}

func TestCreateCampaign(t *testing.T) {
	f := &fakeGraph{status: 200, body: `{"id":"120","name":"Spring","status":"PAUSED"}`}
	c := f.client(t)
	got, err := c.CreateCampaign(context.Background(), "tok", "act_42", CampaignInput{
		Name: "Spring", Objective: "OUTCOME_LEADS", Status: "PAUSED", DailyBudget: 50000,
	})
	if err != nil || got.ID != "120" {
		t.Fatalf("create: %+v %v", got, err)
	}
	if f.method != http.MethodPost || f.path != "/"+GraphVersion+"/act_42/campaigns" {
		t.Errorf("the act_ prefix is added once: %s %s", f.method, f.path)
	}
	if f.form.Get("access_token") != "tok" || f.form.Get("daily_budget") != "50000" || f.form.Get("special_ad_categories") != "[]" || f.form.Get("lifetime_budget") != "" {
		t.Errorf("form: %v", f.form)
	}
	if _, err := c.CreateCampaign(context.Background(), "", "42", CampaignInput{}); err == nil {
		t.Error("a missing token is refused before any call")
	}
}

func TestUpdateDeleteAndErrors(t *testing.T) {
	f := &fakeGraph{status: 200, body: `{"id":"120","status":"ACTIVE"}`}
	c := f.client(t)
	if _, err := c.UpdateCampaign(context.Background(), "tok", "120", CampaignInput{Status: "ACTIVE"}); err != nil {
		t.Fatal(err)
	}
	if f.path != "/"+GraphVersion+"/120" || f.form.Get("status") != "ACTIVE" || f.form.Has("name") {
		t.Errorf("update sends only the set fields: %s %v", f.path, f.form)
	}
	if err := c.DeleteCampaign(context.Background(), "tok", "120"); err != nil || f.method != http.MethodDelete {
		t.Fatalf("delete: %v %s", err, f.method)
	}
	// An id can never climb out of its path segment.
	_ = c.DeleteCampaign(context.Background(), "tok", "../act_1")
	if !strings.HasPrefix(f.path, "/"+GraphVersion+"/..%2Fact_1") {
		t.Errorf("campaign id must be escaped: %s", f.path)
	}

	f.status, f.body = 400, `{"error":{"message":"Invalid parameter"}}`
	if _, err := c.UpdateCampaign(context.Background(), "tok", "120", CampaignInput{Name: "x"}); err == nil || !strings.Contains(err.Error(), "Invalid parameter") {
		t.Errorf("a Graph error is returned with its message: %v", err)
	}
	if err := c.DeleteCampaign(context.Background(), "tok", "120"); err == nil {
		t.Error("a failed delete is an error")
	}
}

func TestGetCampaignInsights(t *testing.T) {
	f := &fakeGraph{status: 200, body: `{"data":[{"campaign_id":"120","date_start":"2026-10-22","impressions":"1000","clicks":"40","spend":"12.50"}]}`}
	c := f.client(t)
	got, err := c.GetCampaignInsights(context.Background(), "tok", "120")
	if err != nil || got.Impressions != "1000" || got.Spend != "12.50" {
		t.Fatalf("insights: %+v %v", got, err)
	}
	if f.path != "/"+GraphVersion+"/120/insights" || f.query.Get("date_preset") != "today" {
		t.Errorf("request: %s %v", f.path, f.query)
	}
	f.body = `{"data":[]}`
	if got, err := c.GetCampaignInsights(context.Background(), "tok", "120"); err != nil || got.CampaignID != "120" || got.Impressions != "" {
		t.Errorf("no row today gives an empty row for the campaign: %+v %v", got, err)
	}
	f.status, f.body = 500, "boom"
	if _, err := c.GetCampaignInsights(context.Background(), "tok", "120"); err == nil {
		t.Error("a 5xx is an error")
	}
}
