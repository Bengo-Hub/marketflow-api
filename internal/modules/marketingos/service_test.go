package marketingos

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"go.uber.org/zap"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/activity"
	"github.com/bengobox/marketflow-api/internal/ent/admetrics"
	"github.com/bengobox/marketflow-api/internal/ent/campaign"
	"github.com/bengobox/marketflow-api/internal/ent/customagent"
	"github.com/bengobox/marketflow-api/internal/ent/deal"
	"github.com/bengobox/marketflow-api/internal/ent/knowledgebaseentry"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
)

const testOSPostgresDSN = "postgres://postgres:postgres@localhost:5432/marketflow?sslmode=disable"

func openDB(t *testing.T) *ent.Client {
	t.Helper()
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", testOSPostgresDSN)
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		t.Skipf("local postgres unreachable: %v", err)
	}
	db := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, sqlDB)))
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func seedTenant(t *testing.T, db *ent.Client) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	tid := uuid.New()
	if _, err := db.Tenant.Create().SetID(tid).SetName("OS " + tid.String()).SetSlug("os-" + tid.String()).
		SetBusinessType("generic").Save(ctx); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	// Cleanup touches only this test's own tenant.
	t.Cleanup(func() {
		_, _ = db.Activity.Delete().Where(activity.TenantID(tid)).Exec(ctx)
		_, _ = db.AdMetrics.Delete().Where(admetrics.TenantID(tid)).Exec(ctx)
		_, _ = db.Deal.Delete().Where(deal.TenantID(tid)).Exec(ctx)
		_, _ = db.Lead.Delete().Where(lead.TenantID(tid)).Exec(ctx)
		_, _ = db.Campaign.Delete().Where(campaign.TenantID(tid)).Exec(ctx)
		_, _ = db.CustomAgent.Delete().Where(customagent.TenantID(tid)).Exec(ctx)
		_, _ = db.KnowledgeBaseEntry.Delete().Where(knowledgebaseentry.TenantID(tid)).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})
	return tid
}

func TestBrandContextValidationAndStorage(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db)
	// Unrelated metadata must survive a save.
	if _, err := db.Tenant.UpdateOneID(tid).SetMetadata(map[string]any{"keep": "me"}).Save(ctx); err != nil {
		t.Fatal(err)
	}
	svc := NewService(db, nil, zap.NewNop())
	if _, err := svc.SaveBrandContext(ctx, tid, BrandContext{Positioning: strings.Repeat("x", maxTextLen+1)}); err == nil {
		t.Fatal("an oversized positioning must be rejected")
	}
	saved, err := svc.SaveBrandContext(ctx, tid, BrandContext{Positioning: "  Shea butter lotion from Kisumu ", Offers: []string{"Shea Butter Body Lotion", " "}})
	if err != nil {
		t.Fatal(err)
	}
	if saved.Positioning != "Shea butter lotion from Kisumu" || len(saved.Offers) != 1 {
		t.Fatalf("not cleaned: %+v", saved)
	}
	got, _ := svc.GetBrandContext(ctx, tid)
	if got.Positioning != saved.Positioning {
		t.Fatalf("get = %+v", got)
	}
	tn, _ := db.Tenant.Get(ctx, tid)
	if tn.Metadata["keep"] != "me" {
		t.Fatal("other metadata was lost")
	}
}

func TestSettingsDefaultOnAndKeepOtherMetadata(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db)
	if _, err := db.Tenant.UpdateOneID(tid).SetMetadata(map[string]any{"keep": "me"}).Save(ctx); err != nil {
		t.Fatal(err)
	}
	svc := NewService(db, nil, zap.NewNop())
	st, err := svc.GetSettings(ctx, tid)
	if err != nil || !st.DailyBrief {
		t.Fatalf("absent switch must read as on: %+v %v", st, err)
	}
	if _, err := svc.SaveSettings(ctx, tid, Settings{DailyBrief: false}); err != nil {
		t.Fatal(err)
	}
	tn, _ := db.Tenant.Get(ctx, tid)
	if tn.Metadata[DailyBriefKey] != false || tn.Metadata["keep"] != "me" {
		t.Fatalf("metadata = %+v", tn.Metadata)
	}
	if st, _ := svc.GetSettings(ctx, tid); st.DailyBrief {
		t.Fatal("saved off, read on")
	}
}

func TestTeamOverridesAndContext(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db)
	svc := NewService(db, nil, zap.NewNop())

	if _, err := svc.UpsertTeamMember(ctx, tid, MemberOverride{Key: "Bad Key"}); err == nil {
		t.Fatal("bad key accepted")
	}
	if _, err := svc.UpsertTeamMember(ctx, tid, MemberOverride{Key: "ads_specialist", Name: "Jev", ModelClass: "premium"}); err != nil {
		t.Fatal(err)
	}
	// Second upsert updates the same row (idempotent on key).
	if _, err := svc.UpsertTeamMember(ctx, tid, MemberOverride{Key: "ads_specialist", Name: "Jev", Disabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpsertTeamMember(ctx, tid, MemberOverride{Key: "manager", Disabled: true}); err != nil {
		t.Fatal(err)
	}
	// A normal CRM agent must not show up as a team member.
	if _, err := db.CustomAgent.Create().SetTenantID(tid).SetName("CRM agent").Save(ctx); err != nil {
		t.Fatal(err)
	}
	members, err := svc.TeamOverrides(ctx, tid)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 || members[0].Key != "ads_specialist" || !members[0].Disabled || members[0].Name != "Jev" {
		t.Fatalf("members = %+v", members)
	}
	if members[1].Key != "manager" || members[1].Disabled {
		t.Fatalf("the manager must stay enabled: %+v", members[1])
	}
	// Saved without a name, the override has none, so the default name stays.
	if members[1].Name != "" {
		t.Fatalf("an unnamed override renamed the manager to %q", members[1].Name)
	}
	// A row saved before metadata carried the name, labelled from its key.
	legacy := map[string]any{"kind": KindTeamMember, "key": "seo_writer"}
	if _, err := db.CustomAgent.Create().SetTenantID(tid).SetName("seo writer").SetTriggerType("manual").SetMetadata(legacy).Save(ctx); err != nil {
		t.Fatal(err)
	}
	members, _ = svc.TeamOverrides(ctx, tid)
	for _, m := range members {
		if m.Key == "seo_writer" && m.Name != "" {
			t.Fatalf("legacy key label read as a rename: %q", m.Name)
		}
	}

	_, _ = svc.SaveBrandContext(ctx, tid, BrandContext{Positioning: "p", Offers: []string{"Gift box", "shea butter body lotion"}})
	for _, title := range []string{"Shea Butter Body Lotion", "Delivery FAQ"} {
		cat := "product"
		if title == "Delivery FAQ" {
			cat = "faq"
		}
		if _, err := db.KnowledgeBaseEntry.Create().SetTenantID(tid).SetScope("tenant").SetCategory(cat).SetTitle(title).SetContent("c").Save(ctx); err != nil {
			t.Fatal(err)
		}
	}
	oc, err := svc.Context(ctx, tid)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(oc.Offerings, "|") != "Shea Butter Body Lotion|Gift box" {
		t.Fatalf("offerings = %v (FAQ excluded, case-insensitive dedupe)", oc.Offerings)
	}
	if oc.BusinessType != "generic" || !strings.Contains(oc.MarketingContext, `"positioning":"p"`) || len(oc.Members) != 3 {
		t.Fatalf("context = %+v", oc)
	}
}

func TestBriefMetricsMatchHandCount(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db)
	svc := NewService(db, nil, zap.NewNop())

	day := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC) // brief day; "yesterday" is Oct 21
	yday := day.AddDate(0, 0, -1).Add(10 * time.Hour)
	camp, err := db.Campaign.Create().SetTenantID(tid).SetName("Lotion ad").Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mkLead := func(at time.Time, fromAd bool) {
		q := db.Lead.Create().SetTenantID(tid).SetSource("website").SetLeadType("customer").SetStatus("new").SetFirstName("L").SetCreatedAt(at)
		if fromAd {
			q.SetCampaignID(camp.ID)
		}
		if _, err := q.Save(ctx); err != nil {
			t.Fatal(err)
		}
	}
	mkLead(yday, true)
	mkLead(yday, true)
	mkLead(yday, false)
	for i := 2; i <= 8; i++ { // one ad lead on each of the 7 baseline days
		mkLead(day.AddDate(0, 0, -i).Add(9*time.Hour), true)
	}
	mkLead(day.Add(2*time.Hour), true) // today: outside both windows

	if _, err := db.AdMetrics.Create().SetTenantID(tid).SetCampaignID(camp.ID).SetPlatform("meta").SetDate(day.AddDate(0, 0, -1)).
		SetSpend(900).SetLeads(2).SetCurrency("KES").Save(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AdMetrics.Create().SetTenantID(tid).SetCampaignID(camp.ID).SetPlatform("meta").SetDate(day.AddDate(0, 0, -3)).
		SetSpend(1400).SetLeads(7).SetCurrency("KES").Save(ctx); err != nil {
		t.Fatal(err)
	}
	m, err := svc.BriefMetrics(ctx, tid, day)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]BriefMetric{}
	for _, x := range m {
		byKey[x.Key] = x
	}
	check := func(key string, value, baseline float64) {
		t.Helper()
		got, ok := byKey[key]
		if !ok {
			t.Fatalf("metric %s missing", key)
		}
		if diff(got.Value, value) || diff(got.Baseline, baseline) {
			t.Fatalf("%s = %v / %v, want %v / %v", key, got.Value, got.Baseline, value, baseline)
		}
	}
	check("leads", 3, 1)
	check("ad_leads", 2, 1)
	check("ad_spend", 900, 200)
	check("cpl", 450, 200) // 900/2 yesterday; baseline 200 KES a day over 1 lead a day
	check("orders", 0, 0)
	if !HasActivity(m) || HasActivity([]BriefMetric{{Key: "x"}}) {
		t.Fatal("HasActivity wrong")
	}
}

func diff(a, b float64) bool {
	d := a - b
	return d > 0.001 || d < -0.001
}
