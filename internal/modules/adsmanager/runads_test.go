package adsmanager

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/campaign"
	"github.com/bengobox/marketflow-api/internal/ent/creativeasset"
	"github.com/bengobox/marketflow-api/internal/ent/creativebrief"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/bengobox/marketflow-api/internal/integrations/google"
	"github.com/bengobox/marketflow-api/internal/platform/runads"
)

const runAdsTestDSN = "postgres://postgres:postgres@localhost:5432/marketflow?sslmode=disable"

func runAdsDB(t *testing.T) *ent.Client {
	t.Helper()
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", runAdsTestDSN)
	if err != nil || sqlDB.Ping() != nil {
		t.Skip("local postgres unreachable")
	}
	db := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, sqlDB)))
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// runAdsFixture is a new tenant with an approved video and the given run_ads settings. Cleanup
// removes only that tenant's rows.
func runAdsFixture(t *testing.T, db *ent.Client, settings map[string]any) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	tid := uuid.New()
	db.Tenant.Create().SetID(tid).SetName("Run ads " + tid.String()[:8]).SetSlug("ra-" + tid.String()).
		SetMetadata(map[string]any{runads.TenantKey: settings}).ExecX(ctx)
	b := db.CreativeBrief.Create().SetTenantID(tid).SetTitle("Soap").SaveX(ctx)
	v := db.CreativeVersion.Create().SetTenantID(tid).SetBriefID(b.ID).SetVersionNo(1).SetKind("video_ad").SetFormat("9:16").SetStatus("approved").
		SetCopy(map[string]any{"headline": "Soft skin soap", "subline": "Made in Nairobi"}).SaveX(ctx)
	db.CreativeAsset.Create().SetTenantID(tid).SetVersionID(v.ID).SetKind("video").SetStorageKey("k/" + v.ID.String()).
		SetURL("https://media.example.com/" + v.ID.String() + ".mp4").SaveX(ctx)
	t.Cleanup(func() {
		_, _ = db.Campaign.Delete().Where(campaign.TenantIDEQ(tid)).Exec(ctx)
		_, _ = db.CreativeAsset.Delete().Where(creativeasset.TenantIDEQ(tid)).Exec(ctx)
		_, _ = db.CreativeVersion.Delete().Where(creativeversion.TenantIDEQ(tid)).Exec(ctx)
		_, _ = db.CreativeBrief.Delete().Where(creativebrief.TenantIDEQ(tid)).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})
	return tid, v.ID
}

func runAdsHandler(db *ent.Client) *Handler {
	now := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	return &Handler{DB: db, Logger: zap.NewNop(), RunAds: &RunAdsDeps{
		Currency: func(context.Context, uuid.UUID) string { return "KES" },
		Now:      func() time.Time { return now },
	}}
}

func postJSON(h http.HandlerFunc, tid uuid.UUID, body any, id string) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(b))
	r.Header.Set("X-Tenant-ID", tid.String())
	if id != "" {
		rc := chi.NewRouteContext()
		rc.URLParams.Add("id", id)
		r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rc))
	}
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

// withAdvertiser connects a TikTok advertiser to the fixture's tenant.
func withAdvertiser(t *testing.T, db *ent.Client, tid uuid.UUID, currency string) {
	t.Helper()
	ctx := context.Background()
	a := db.ConnectedAccount.Create().SetTenantID(tid).SetPlatform("tiktok").SetAccountType("tiktok_advertiser").SetExternalID("7123").
		SetDisplayName("Shop").SetStatus("active").SetMetadata(map[string]any{"currency": currency}).SaveX(ctx)
	t.Cleanup(func() { _ = db.ConnectedAccount.DeleteOneID(a.ID).Exec(ctx) })
}

// Without an advertiser TikTok is left out of the default plan and its total (owner, 2026-11-25).
func TestRunAdsTikTokWithoutAdvertiserLeftOut(t *testing.T) {
	db := runAdsDB(t)
	tid, vid := runAdsFixture(t, db, map[string]any{"platforms": []any{"tiktok"}, "daily_budget": map[string]any{"tiktok": float64(4000)}})
	plan, _, err := runAdsHandler(db).BuildPlan(context.Background(), tid, vid, Choice{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Platforms[2].Enabled || plan.Total != 0 || len(plan.Platforms[2].Setup) == 0 {
		t.Fatalf("plan = %+v", plan.Platforms[2])
	}
}

// Plans are in USD: amounts saved in KES are converted (never relabelled), the tenant currency is
// shown beside, the account gets its own currency, and an amount typed in another currency is
// converted too. Without the rate the plan stays in the tenant's currency and says so.
func TestRunAdsInUSD(t *testing.T) {
	db := runAdsDB(t)
	tid, vid := runAdsFixture(t, db, map[string]any{"platforms": []any{"tiktok"}, "days": float64(5), "total_cap": float64(26000),
		"daily_budget": map[string]any{"tiktok": float64(5000)}})
	withAdvertiser(t, db, tid, "KES")
	h := runAdsHandler(db)
	ctx := context.Background()
	h.KESPer = func(_ context.Context, c string) (float64, error) {
		if c == "USD" {
			return 130, nil
		}
		return 0, errNoRate
	}
	plan, _, err := h.BuildPlan(ctx, tid, vid, Choice{})
	if err != nil {
		t.Fatal(err)
	}
	tk := plan.Platforms[2]
	if plan.Currency != "USD" || tk.Daily != 38 || plan.TotalCap != 200 || plan.Total != 190 {
		t.Fatalf("plan = %s daily %d cap %d total %d", plan.Currency, tk.Daily, plan.TotalCap, plan.Total)
	}
	if plan.LocalCurrency != "KES" || plan.LocalTotal != 24700 || tk.LocalDaily != 4940 || tk.AccountDaily != 4940 {
		t.Fatalf("local %s %d, daily %d, account %d", plan.LocalCurrency, plan.LocalTotal, tk.LocalDaily, tk.AccountDaily)
	}
	if !strings.Contains(PlanText(plan), "USD 190 (about KES 24,700)") {
		t.Fatalf("text: %s", PlanText(plan))
	}
	zero := int64(0)
	plan, _, _ = h.BuildPlan(ctx, tid, vid, Choice{Currency: "KES", TotalCap: &zero, Platforms: []PlatformChoice{{Platform: "tiktok", Daily: 13000}}})
	if plan.Platforms[2].Daily != 100 || plan.TotalCap != 0 {
		t.Fatalf("typed in KES: daily %d cap %d", plan.Platforms[2].Daily, plan.TotalCap)
	}
	h.KESPer = nil
	plan, _, _ = h.BuildPlan(ctx, tid, vid, Choice{})
	if plan.Currency != "KES" || plan.Platforms[2].Daily != 5000 || len(plan.Notes) != 1 {
		t.Fatalf("without a rate: %s %d %v", plan.Currency, plan.Platforms[2].Daily, plan.Notes)
	}
	// An amount typed in USD is not read as KES when the rate is gone.
	plan, _, _ = h.BuildPlan(ctx, tid, vid, Choice{Currency: "USD", Platforms: []PlatformChoice{{Platform: "tiktok", Daily: 40}}})
	if plan.Ready || plan.Platforms[2].Daily == 40 {
		t.Fatalf("USD read as KES: %+v", plan.Platforms[2])
	}
}

// A TikTok-only plan is guided: the owner's amount, the run days, steps with links, nothing spent
// until the run; the run records one guided campaign and a second confirm replays it.
func TestRunAdsTikTokGuidedOnce(t *testing.T) {
	db := runAdsDB(t)
	tid, vid := runAdsFixture(t, db, map[string]any{"platforms": []any{"tiktok"}, "days": float64(5),
		"daily_budget": map[string]any{"tiktok": float64(4000)}})
	withAdvertiser(t, db, tid, "KES")
	h := runAdsHandler(db)
	ctx := context.Background()

	plan, _, err := h.BuildPlan(ctx, tid, vid, Choice{})
	if err != nil {
		t.Fatal(err)
	}
	tk := plan.Platforms[2]
	if plan.Platforms[0].Enabled || plan.Platforms[1].Enabled || !tk.Enabled || tk.Mode != "guided" {
		t.Fatalf("platforms = %+v", plan.Platforms)
	}
	if tk.Daily != 4000 || tk.Tier != runads.TierCustom || plan.Total != 20000 {
		t.Fatalf("plan = %+v tiktok = %+v", plan, tk)
	}
	if !plan.Ready {
		t.Fatalf("not ready: %v %v", plan.Blockers, tk.Blockers)
	}
	if tk.Funding.CanSpend != nil || !strings.Contains(tk.Funding.Link, "ads.tiktok.com") || len(tk.Steps) < 3 {
		t.Fatalf("tiktok funding/steps = %+v / %+v", tk.Funding, tk.Steps)
	}

	if w := postJSON(h.Run, tid, RunInput{VersionID: vid.String(), Fingerprint: "stale"}, ""); w.Code != http.StatusConflict {
		t.Fatalf("a stale plan ran: %d %s", w.Code, w.Body)
	}
	w := postJSON(h.Run, tid, RunInput{VersionID: vid.String(), Fingerprint: plan.Fingerprint}, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("run = %d %s", w.Code, w.Body)
	}
	var out RunOutcome
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if len(out.Results) != 1 || out.Results[0].Status != "guided" || out.Results[0].CampaignID == "" {
		t.Fatalf("results = %+v", out.Results)
	}
	again := postJSON(h.Run, tid, RunInput{VersionID: vid.String(), Fingerprint: plan.Fingerprint}, "")
	if again.Code != http.StatusOK || !strings.Contains(again.Body.String(), `"replayed":true`) {
		t.Fatalf("second confirm = %d %s", again.Code, again.Body)
	}
	if n := db.Campaign.Query().Where(campaign.TenantIDEQ(tid)).CountX(ctx); n != 1 {
		t.Fatalf("%d campaign rows, want 1", n)
	}
	row := db.Campaign.Query().Where(campaign.TenantIDEQ(tid)).OnlyX(ctx)
	if row.Status != "guided" || row.Platform != "tiktok" || row.BudgetTotal == nil || *row.BudgetTotal != 20000 {
		t.Fatalf("row = %s %s %v", row.Status, row.Platform, row.BudgetTotal)
	}
	if w := postJSON(h.TikTokDone, tid, nil, row.ID.String()); w.Code != http.StatusOK {
		t.Fatalf("done = %d %s", w.Code, w.Body)
	}
}

// The tenant's cap stops a plan that totals more, and no platform means nothing to confirm.
func TestRunAdsCapAndNothingOn(t *testing.T) {
	db := runAdsDB(t)
	tid, vid := runAdsFixture(t, db, map[string]any{"platforms": []any{"tiktok"}, "days": float64(10), "total_cap": float64(25000),
		"daily_budget": map[string]any{"tiktok": float64(3000)}})
	withAdvertiser(t, db, tid, "KES")
	h := runAdsHandler(db)
	plan, _, err := h.BuildPlan(context.Background(), tid, vid, Choice{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Ready || len(plan.Blockers) != 1 || !strings.Contains(plan.Blockers[0], "over your total cap") {
		t.Fatalf("cap: %+v", plan.Blockers)
	}
	off := false
	ch := Choice{Days: 5, Platforms: []PlatformChoice{{Platform: "tiktok", Enabled: &off}}}
	plan, _, _ = h.BuildPlan(context.Background(), tid, vid, ch)
	if plan.Ready || plan.Total != 0 {
		t.Fatalf("nothing on: %+v", plan)
	}
	if w := postJSON(h.Run, tid, RunInput{VersionID: vid.String(), Fingerprint: plan.Fingerprint, Choice: ch}, ""); w.Code != http.StatusConflict {
		t.Fatalf("an empty plan ran: %d", w.Code)
	}
}

func TestRunAdsNeedsApproval(t *testing.T) {
	db := runAdsDB(t)
	tid, vid := runAdsFixture(t, db, nil)
	db.CreativeVersion.UpdateOneID(vid).SetStatus("ready").ExecX(context.Background())
	h := runAdsHandler(db)
	if w := postJSON(h.Plan, tid, RunInput{VersionID: vid.String()}, ""); w.Code != http.StatusConflict {
		t.Fatalf("plan for an unapproved video = %d", w.Code)
	}
}

func TestPlanTextAndHelpers(t *testing.T) {
	no := false
	p := &RunPlan{Days: 7, Currency: "KES", Total: 21000, Platforms: []PlatformPlan{
		{Platform: runads.Meta, Enabled: true, Daily: 3000, Estimates: []runads.Estimate{{Metric: "reach", Value: 12000, Per: "day", Source: runads.SourcePlatform}},
			Funding: FundingView{CanSpend: &no, Detail: "Add KES 500 on Meta", Link: "https://business.facebook.com/x"}},
		{Platform: runads.Google, Enabled: false},
	}}
	txt := PlanText(p)
	for _, want := range []string{"total KES 21,000", "Meta: KES 3,000 a day", "About 12,000 reach a day (the platform's estimate)",
		"Add money: https://business.facebook.com/x", "Nothing is spent until you confirm"} {
		if !strings.Contains(txt, want) {
			t.Errorf("plan text lacks %q:\n%s", want, txt)
		}
	}
	if strings.Contains(txt, "Google") {
		t.Error("a platform that is off is in the text")
	}
	if thousands(1234567) != "1,234,567" || thousands(12) != "12" {
		t.Fatal("thousands")
	}
	if got := clip("Fresh bread from our Nairobi bakery every morning", 30); got != "Fresh bread from our Nairobi" {
		t.Fatalf("clip = %q", got)
	}
}

func TestGoogleTextsFromCopyOnly(t *testing.T) {
	pc := &planCtx{tenant: &ent.Tenant{Name: "Mama Soap", Metadata: map[string]any{"tagline": "Handmade soap"}},
		version: &ent.CreativeVersion{Copy: map[string]any{"headline": "Soft skin soap", "subline": "Made in Nairobi", "cta": "Order on WhatsApp"}}}
	head, long, desc := googleTexts(pc, "https://www.mamasoap.co.ke/shop")
	if len(head) < 3 || head[0] != "Soft skin soap" || head[len(head)-1] != "Visit mamasoap.co.ke" {
		t.Fatalf("headlines = %v", head)
	}
	if len(long) != 1 || long[0] != "Soft skin soap Made in Nairobi" || len(desc) < 2 {
		t.Fatalf("long = %v desc = %v", long, desc)
	}
}

// A platform still to be set up is left out of the default plan, so one ready platform can run.
func TestRunAdsLeavesOutWhatIsNotSetUp(t *testing.T) {
	db := runAdsDB(t)
	tid, vid := runAdsFixture(t, db, map[string]any{"daily_budget": map[string]any{"meta": float64(1000), "google": float64(1000), "tiktok": float64(4000)}})
	withAdvertiser(t, db, tid, "KES")
	h := runAdsHandler(db)
	plan, _, err := h.BuildPlan(context.Background(), tid, vid, Choice{})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Ready || plan.Platforms[0].Enabled || plan.Platforms[1].Enabled || plan.Total != 4000*int64(plan.Days) {
		t.Fatalf("plan = %+v", plan)
	}
	if !strings.Contains(PlanText(plan), "Not included yet, Google Ads") {
		t.Fatalf("text: %s", PlanText(plan))
	}
	on := true
	plan, _, _ = h.BuildPlan(context.Background(), tid, vid, Choice{Platforms: []PlatformChoice{{Platform: "google", Enabled: &on}}})
	if plan.Ready {
		t.Fatal("a platform the owner switched on but did not set up must block")
	}
}

// A safe failure can be confirmed again by one caller; finished platforms are kept, and an ended
// run gives the same plan a new fingerprint.
func TestRunAdsPartialClaim(t *testing.T) {
	db := runAdsDB(t)
	tid, vid := runAdsFixture(t, db, nil)
	h := runAdsHandler(db)
	ctx := context.Background()
	if ok, err := h.claimRun(ctx, tid, vid, "fp", ""); err != nil || !ok {
		t.Fatalf("first claim %v %v", ok, err)
	}
	if ok, _ := h.claimRun(ctx, tid, vid, "fp", ""); ok {
		t.Fatal("a running plan was claimed twice")
	}
	h.saveRun(ctx, tid, vid, "fp", map[string]any{"status": runPartial, "at": "t1", "ends_at": "2026-11-20T00:00:00Z",
		"results": []PlatformResult{{Platform: "meta", Status: "created"}, {Platform: "google", Status: "failed"}}})
	if _, ok := h.storedRun(ctx, tid, vid, "fp"); ok {
		t.Fatal("a partial run must not replay as finished")
	}
	res, at := h.partialRun(ctx, tid, vid, "fp")
	if at != "t1" || len(res) != 2 || res[0].retryable() || !res[1].retryable() {
		t.Fatalf("partial = %+v %q", res, at)
	}
	if ok, err := h.claimRun(ctx, tid, vid, "fp", at); err != nil || !ok {
		t.Fatalf("continue %v %v", ok, err)
	}
	if ok, _ := h.claimRun(ctx, tid, vid, "fp", at); ok {
		t.Fatal("a continuation was claimed twice")
	}
	h.saveRun(ctx, tid, vid, "fp", map[string]any{"status": runDone, "ends_at": "2026-11-20T00:00:00Z"})
	v := db.CreativeVersion.GetX(ctx, vid)
	if n := endedRuns(v.Metadata, h.RunAds.now()); n != 1 {
		t.Fatalf("ended = %d", n)
	}
	p := &RunPlan{VersionID: vid.String()}
	if planFingerprint(p, 0) == planFingerprint(p, 1) {
		t.Fatal("an ended run must allow a new one")
	}
}

// Google end to end on fakes: a lookup that fails creates nothing; then one campaign total is
// created; a second launch makes no second campaign.
func TestLaunchGoogleOnce(t *testing.T) {
	db := runAdsDB(t)
	tid, vid := runAdsFixture(t, db, nil)
	h := runAdsHandler(db)
	ctx := context.Background()
	var png bytes.Buffer
	_ = jpeg.Encode(&png, image.NewRGBA(image.Rect(0, 0, 1300, 800)), nil)
	creates, searchFails := 0, true
	var sent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".jpg"):
			_, _ = w.Write(png.Bytes())
		case strings.HasSuffix(r.URL.Path, "googleAds:search"):
			if searchFails {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(`{"results":[]}`))
		case strings.HasSuffix(r.URL.Path, "googleAds:mutate"):
			creates++
			b := new(bytes.Buffer)
			_, _ = b.ReadFrom(r.Body)
			sent = b.String()
			_, _ = w.Write([]byte(`{"mutateOperationResponses":[{"campaignResult":{"resourceName":"customers/1/campaigns/7"}}]}`))
		}
	}))
	defer srv.Close()
	h.RunAds.Fetch = srv.Client()
	pc := &planCtx{tenant: db.Tenant.GetX(ctx, tid), version: db.CreativeVersion.GetX(ctx, vid), settings: runads.Fallback(), cur: "KES"}
	pc.google = &googleTarget{acc: &ent.ConnectedAccount{ID: uuid.New()}, customer: "1", timeZone: "Africa/Nairobi", youtubeIDs: []string{"yt1"},
		finalURL: "https://soap.test", logoURL: srv.URL + "/logo.jpg", pictures: []string{srv.URL + "/p.jpg"},
		ads: google.Ads{HTTP: srv.Client(), Base: srv.URL, Version: "v26", AccessToken: "t"}}
	pp := PlatformPlan{Platform: runads.Google, Enabled: true, Daily: 1000, Days: 7, Total: 7000, AccountDaily: 1000, AccountTotal: 7000}
	key := strings.Repeat("a", 32)
	if res := h.launchGoogle(ctx, tid, pc, pp, key); res.Status != "failed" || creates != 0 {
		t.Fatalf("lookup failure: %+v creates %d", res, creates)
	}
	searchFails = false
	res := h.launchGoogle(ctx, tid, pc, pp, key)
	if res.Status != "created" || res.External != "customers/1/campaigns/7" || creates != 1 {
		t.Fatalf("create: %+v creates %d", res, creates)
	}
	if !strings.Contains(sent, `"totalAmountMicros":"7000000000"`) || !strings.Contains(sent, "CUSTOM_PERIOD") || strings.Contains(sent, `"amountMicros"`) {
		t.Fatal("the budget must be the confirmed total")
	}
	if res := h.launchGoogle(ctx, tid, pc, pp, key); res.Status != "created" || creates != 1 {
		t.Fatalf("second launch: %+v creates %d", res, creates)
	}
	if n := db.Campaign.Query().Where(campaign.TenantIDEQ(tid)).CountX(ctx); n != 1 {
		t.Fatalf("%d rows", n)
	}
}

func TestCropJPEG(t *testing.T) {
	b, err := cropJPEG(image.NewRGBA(image.Rect(0, 0, 2000, 1500)), 1.91, 600, 314)
	if err != nil {
		t.Fatal(err)
	}
	out, err := jpeg.Decode(bytes.NewReader(b))
	if err != nil || out.Bounds().Dx() != 1200 || out.Bounds().Dy() != 628 {
		t.Fatalf("landscape = %v %v", out.Bounds(), err)
	}
	if _, err := cropJPEG(image.NewRGBA(image.Rect(0, 0, 100, 100)), 1, 128, 128); err == nil {
		t.Fatal("a 100 pixel logo was accepted")
	}
}

func TestCompetitorDays(t *testing.T) {
	meta := map[string]any{"competitor_scan": map[string]any{"competitors": []any{
		map[string]any{"winners": []any{
			map[string]any{"still_running": true, "total_days_shown": float64(40)},
			map[string]any{"still_running": false, "total_days_shown": float64(400)},
			map[string]any{"still_running": true, "total_days_shown": float64(12)},
			map[string]any{"still_running": true, "total_days_shown": float64(20)},
		}}}}}
	if d := competitorDays(meta); d != 20 {
		t.Fatalf("median = %d", d)
	}
}
