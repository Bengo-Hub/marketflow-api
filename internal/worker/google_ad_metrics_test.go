package worker

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent/admetrics"
	"github.com/bengobox/marketflow-api/internal/ent/campaign"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/ent/customagent"
	"github.com/bengobox/marketflow-api/internal/integrations/google"
	"github.com/bengobox/marketflow-api/internal/modules/custom_agents"
	"github.com/bengobox/marketflow-api/internal/modules/googleads"
)

// A Run ads Google campaign's days are stored beside Meta's, a second read replaces them, and the
// agent's month to date spend converts them into its ceiling's currency.
func TestGoogleAdMetrics(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db, "google-metrics")
	t.Cleanup(func() {
		_, _ = db.AdMetrics.Delete().Where(admetrics.TenantID(tid)).Exec(ctx)
		_, _ = db.Campaign.Delete().Where(campaign.TenantID(tid)).Exec(ctx)
		_, _ = db.ConnectedAccount.Delete().Where(connectedaccount.TenantID(tid)).Exec(ctx)
		_, _ = db.CustomAgent.Delete().Where(customagent.TenantID(tid)).Exec(ctx)
	})
	now := time.Now().UTC()
	day := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
	cost := "2500000"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), "customers/55/campaigns/7") {
			t.Errorf("query %s", b)
		}
		_, _ = w.Write([]byte(`{"results":[{"campaign":{"id":"7","name":"x"},"segments":{"date":"` + day +
			`"},"metrics":{"impressions":"100","clicks":"10","conversions":2,"costMicros":"` + cost + `"}}]}`))
	}))
	defer srv.Close()
	acc := db.ConnectedAccount.Create().SetTenantID(tid).SetPlatform("google").SetAccountType(googleads.AccountType).SetExternalID("55").
		SetDisplayName("Ads").SetStatus("active").SetMetadata(map[string]any{"currency": "USD",
		google.MetaAccessExpiresAt: now.Add(time.Hour).Format(time.RFC3339)}).SaveX(ctx)
	c := db.Campaign.Create().SetTenantID(tid).SetName("Run ads google").SetCampaignType("lead_gen").SetPlatform("google").SetStatus("active").
		SetCurrency("KES").SetConnectedAccountID(acc.ID).SetEndDate(now.AddDate(0, 0, 3)).
		SetMetadata(map[string]any{"google": map[string]any{"campaign_resource": "customers/55/campaigns/7"}}).SaveX(ctx)
	j := &GoogleAdMetrics{db: db, hc: srv.Client(), base: srv.URL, now: time.Now, logger: zap.NewNop()}
	ads := google.AdsFromSettings(srv.Client(), nil, "tok")
	ads.Base = srv.URL
	for range 2 {
		if err := j.store(ctx, c, "USD", ads, "55", "customers/55/campaigns/7"); err != nil {
			t.Fatal(err)
		}
	}
	rows := db.AdMetrics.Query().Where(admetrics.TenantID(tid)).AllX(ctx)
	if len(rows) != 1 || rows[0].Spend != 2.5 || rows[0].Currency != "USD" || rows[0].Platform != "google" || rows[0].Leads != 2 {
		t.Fatalf("rows %+v", rows)
	}
	cost = "4000000"
	if err := j.store(ctx, c, "USD", ads, "55", "customers/55/campaigns/7"); err != nil {
		t.Fatal(err)
	}
	if rows = db.AdMetrics.Query().Where(admetrics.TenantID(tid)).AllX(ctx); len(rows) != 1 || rows[0].Spend != 4 {
		t.Fatalf("corrected rows %+v", rows)
	}
	if err := j.store(ctx, c, "USD", ads, "55", "customers/55/campaigns/7' OR 1"); err == nil {
		t.Fatal("a malformed resource was queried")
	}

	agent := db.CustomAgent.Create().SetTenantID(tid).SetName("Paid").SetTriggerType(custom_agents.TriggerSchedule).SaveX(ctx)
	brief := db.CreativeBrief.Create().SetTenantID(tid).SetTitle("B").SetMetadata(map[string]any{"autopilot_agent_id": agent.ID.String()}).SaveX(ctx)
	v := db.CreativeVersion.Create().SetTenantID(tid).SetBriefID(brief.ID).SetVersionNo(1).SetFormat("1080x1920").SaveX(ctx)
	t.Cleanup(func() {
		_ = db.CreativeVersion.DeleteOneID(v.ID).Exec(ctx)
		_ = db.CreativeBrief.DeleteOneID(brief.ID).Exec(ctx)
	})
	db.Campaign.UpdateOneID(c.ID).SetCreativeVersionID(v.ID).ExecX(ctx)
	conv := func(_ context.Context, amount float64, from, to string) (float64, error) { return amount * 130, nil }
	if got, err := custom_agents.AdSpendThisMonth(ctx, db, tid, agent.ID, now, "KES", conv); err != nil || got != 520 {
		t.Fatalf("month to date %v %v", got, err)
	}
	if _, err := custom_agents.AdSpendThisMonth(ctx, db, tid, agent.ID, now, "KES", nil); err == nil {
		t.Fatal("USD spend counted as KES")
	}
}
