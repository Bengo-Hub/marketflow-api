package publishing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/ent/publishjob"
	"github.com/bengobox/marketflow-api/internal/ent/serviceconfig"
)

var fbScopesOK = []string{"pages_show_list", "pages_read_engagement", "pages_manage_posts"}

// Accounts the video cannot go to are skipped with the reason the publish dialog would give; ad
// accounts are left out entirely.
func TestPickTargets(t *testing.T) {
	ok := Video{DurationMS: 30_000, Width: 1080, Height: 1920, FPS: 30, Bytes: 20 << 20}
	accounts := []*ent.ConnectedAccount{
		{ID: uuid.New(), AccountType: "facebook_page", DisplayName: "Boi Page", Scopes: fbScopesOK},
		{ID: uuid.New(), AccountType: "facebook_page", DisplayName: "Old Page", Scopes: []string{"pages_show_list"}},
		{ID: uuid.New(), AccountType: "tiktok_creator", DisplayName: "Boi TikTok", Scopes: []string{"video.publish"}},
		{ID: uuid.New(), AccountType: "meta_ad_account", DisplayName: "Ads"},
	}
	enabled := func(p string) bool { return p == Facebook }
	targets, skipped := pickTargets(accounts, enabled, ok, Post{Caption: "Weekend offer"})
	if len(targets) != 1 || targets[0].acc.DisplayName != "Boi Page" || targets[0].platform != Facebook {
		t.Fatalf("targets: %+v", targets)
	}
	reasons := map[string]string{}
	for _, s := range skipped {
		reasons[s.DisplayName] = s.Reason
	}
	if len(skipped) != 2 || !strings.Contains(reasons["Old Page"], "reconnecting") || !strings.Contains(reasons["Boi TikTok"], "not switched on") {
		t.Fatalf("skipped: %+v", skipped)
	}
	if _, ok := reasons["Ads"]; ok {
		t.Fatal("an ad account never takes an organic post")
	}
	// A video that fails the preflight is skipped with the problem.
	_, skipped = pickTargets(accounts[:1], enabled, Video{DurationMS: 2000, Width: 1080, Height: 1920}, Post{})
	if len(skipped) != 1 || !strings.Contains(skipped[0].Reason, "at least 3 s") {
		t.Fatalf("preflight: %+v", skipped)
	}
}

func osTurnPublish(h *Handler, body map[string]any) (int, map[string]any) {
	b, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	h.OSTurnPublish(w, httptest.NewRequest(http.MethodPost, "/api/v1/internal/creative/os-turn/publish", bytes.NewReader(b)))
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// seedVideo makes a tenant with a finished video ad, a connected Facebook Page and the Facebook
// switch on.
func seedVideo(t *testing.T, db *ent.Client) (tenant, version uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	tenant = uuid.New()
	db.Tenant.Create().SetID(tenant).SetName("wa " + tenant.String()).SetSlug("wa-" + tenant.String()).
		SetMetadata(map[string]any{BestTimeTenantKey: "off"}).SaveX(ctx)
	brief := db.CreativeBrief.Create().SetTenantID(tenant).SetTitle("Weekend offer").SaveX(ctx)
	v := db.CreativeVersion.Create().SetTenantID(tenant).SetBriefID(brief.ID).SetVersionNo(1).SetKind("video").
		SetFormat("1080x1920").SetStatus("ready").SetCopy(map[string]any{"caption": "Shea butter, 20% off this weekend"}).SaveX(ctx)
	db.CreativeAsset.Create().SetTenantID(tenant).SetVersionID(v.ID).SetKind("video").SetStorageKey("k/ad.mp4").
		SetURL("https://media.example/ad.mp4").SetWidth(1080).SetHeight(1920).SetDurationMs(30_000).SetBytes(20 << 20).
		SetMetadata(map[string]any{"fps": 30.0}).SaveX(ctx)
	db.ConnectedAccount.Create().SetTenantID(tenant).SetPlatform("meta").SetAccountType("facebook_page").
		SetExternalID("page-" + tenant.String()).SetDisplayName("Boi Page").SetScopes(fbScopesOK).SetTokenCiphertext("x").
		SetStatus("active").SaveX(ctx)
	// The platform switch is one shared row; a second run finds it already there.
	_, _ = db.ProviderSetting.Create().SetTenantID("platform").SetProviderType("publishing").SetProviderName("publishing").
		SetKey("facebook_enabled").SetValue("true").SetIsActive(true).Save(ctx)
	return tenant, v.ID
}

// The route approves the video for the user and posts it once per account; a repeat posts nothing
// new; a user without an approving role and a flagged ad post nothing.
func TestOSTurnPublishDB(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	tenant, version := seedVideo(t, db)
	user := uuid.New()
	approver := true
	var approvedBy uuid.UUID
	flagged := []string(nil)
	h := &Handler{Store: &EntStore{DB: db}, Logger: zap.NewNop(), WhatsApp: WhatsAppPublishDeps{
		IsApprover:  func(_ context.Context, tid, uid uuid.UUID) bool { return approver && tid == tenant && uid == user },
		NeedsReview: func(*ent.CreativeVersion) []string { return flagged },
		Approve: func(ctx context.Context, tid, vid, by uuid.UUID) error {
			approvedBy = by
			return db.CreativeVersion.UpdateOneID(vid).SetStatus("approved").Exec(ctx)
		},
	}}
	body := map[string]any{"tenant_id": tenant.String(), "version_id": version.String(), "user_id": user.String(),
		"run_id": "run-1", "sender": "+254700000001"}

	approver = false
	if code, _ := osTurnPublish(h, body); code != http.StatusForbidden {
		t.Fatalf("no role: %d", code)
	}
	approver, flagged = true, []string{"it touches a restricted category (health)"}
	if code, out := osTurnPublish(h, body); code != http.StatusConflict || out["code"] != "needs_review" {
		t.Fatalf("flagged: %d %v", code, out)
	}
	if approvedBy != uuid.Nil {
		t.Fatal("a flagged ad must not be approved")
	}
	flagged = nil
	code, out := osTurnPublish(h, body)
	posted, _ := out["posted"].([]any)
	if code != http.StatusOK || len(posted) != 1 || approvedBy != user {
		t.Fatalf("publish: %d %v (approved by %s)", code, out, approvedBy)
	}
	jobs := db.PublishJob.Query().Where(publishjob.TenantIDEQ(tenant)).AllX(ctx)
	if len(jobs) != 1 || jobs[0].Platform != Facebook || jobs[0].Caption != "Shea butter, 20% off this weekend" ||
		jobs[0].CreatedBy == nil || *jobs[0].CreatedBy != user || jobs[0].Metadata["requested_via"] != "whatsapp" {
		t.Fatalf("jobs: %+v", jobs)
	}
	wa, _ := jobs[0].Metadata[MetaWhatsApp].(map[string]any)
	if wa["to"] != "+254700000001" || wa["ref"] != "run-1:"+version.String() {
		t.Fatalf("whatsapp requester: %v", jobs[0].Metadata)
	}
	if !jobs[0].ScheduledAt.Before(time.Now().Add(time.Minute)) {
		t.Fatal("a WhatsApp post goes now")
	}
	if code, _ := osTurnPublish(h, body); code != http.StatusOK || db.PublishJob.Query().Where(publishjob.TenantIDEQ(tenant)).CountX(ctx) != 1 {
		t.Fatal("a repeat must not post twice")
	}
}

// A post goes now unless the business or the platform turned best time on.
func TestBestTimeIsOffByDefault(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	q := db.ServiceConfig.Query().Where(serviceconfig.ConfigKey(BestTimeConfigKey), serviceconfig.TenantIDIsNil())
	if q.ExistX(ctx) {
		t.Skip("the local database has its own platform row")
	}
	for value, want := range map[string]bool{"": false, "off": false, "maybe": false, "on": true} {
		if got := bestTimeOn(ctx, db, &ent.Tenant{Metadata: map[string]any{BestTimeTenantKey: value}}); got != want {
			t.Errorf("%q: %v", value, got)
		}
	}
	tenant, version := seedVideo(t, db)
	db.Tenant.UpdateOneID(tenant).SetMetadata(map[string]any{}).ExecX(ctx)
	p, err := (&Handler{Store: &EntStore{DB: db}, Logger: zap.NewNop()}).BuildPostPlan(ctx, tenant, version, time.Date(2026, 11, 22, 22, 0, 0, 0, time.UTC))
	if err != nil || p.BestTime || p.Posts[0].At != nil || !strings.Contains(PostPlanText(p), "Boi Page (Facebook), now") {
		t.Fatalf("plan %+v %v", p, err)
	}
}

// The approval covers exactly the plan its message showed: the caption with the contacts and the
// link, the accounts and their best times. A plan that changed is shown again and posts nothing.
func TestPostPlanIsWhatIsPosted(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	tenant, version := seedVideo(t, db)
	db.Tenant.UpdateOneID(tenant).SetMetadata(map[string]any{"ai_label": "off", BestTimeTenantKey: "on"}).ExecX(ctx)
	caption := "Shea butter\n\nWhatsApp: +254700000001\n\nhttps://app.example/f/shea"
	h := &Handler{Store: &EntStore{DB: db}, Logger: zap.NewNop(),
		Caption: func(context.Context, uuid.UUID, uuid.UUID) (string, error) { return caption, nil },
		WhatsApp: WhatsAppPublishDeps{
			IsApprover: func(context.Context, uuid.UUID, uuid.UUID) bool { return true },
			Approve: func(ctx context.Context, _, vid, _ uuid.UUID) error {
				return db.CreativeVersion.UpdateOneID(vid).SetStatus("approved").Exec(ctx)
			},
		}}
	// A Sunday night, so the best time is later.
	now := time.Date(2026, 11, 22, 22, 0, 0, 0, time.UTC)
	p, err := h.BuildPostPlan(ctx, tenant, version, now)
	if err != nil || len(p.Posts) != 1 || p.Posts[0].At == nil || p.AIGenerated || !p.BestTime || p.Caption != caption {
		t.Fatalf("plan %+v %v", p, err)
	}
	text := PostPlanText(p)
	if !strings.Contains(text, "Boi Page (Facebook), ") || !strings.Contains(text, "AI label: off") {
		t.Fatalf("text %q", text)
	}
	again, _ := h.BuildPostPlan(ctx, tenant, version, now.Add(5*time.Minute))
	if again.Fingerprint != p.Fingerprint {
		t.Fatal("the same plan a few minutes later must keep its fingerprint")
	}
	body := map[string]any{"tenant_id": tenant.String(), "version_id": version.String(), "user_id": uuid.NewString(), "run_id": "r", "fingerprint": "stale"}
	code, out := osTurnPublish(h, body)
	details, _ := out["details"].(map[string]any)
	if code != http.StatusConflict || out["code"] != "plan_changed" || details["fingerprint"] == "" {
		t.Fatalf("stale plan: %d %v", code, out)
	}
	if n := db.PublishJob.Query().Where(publishjob.TenantIDEQ(tenant)).CountX(ctx); n != 0 {
		t.Fatalf("a stale plan posted %d", n)
	}
	body["fingerprint"] = details["fingerprint"]
	if code, out := osTurnPublish(h, body); code != http.StatusOK {
		t.Fatalf("publish %d %v", code, out)
	}
	j := db.PublishJob.Query().Where(publishjob.TenantIDEQ(tenant)).OnlyX(ctx)
	if j.Caption != caption || j.Metadata["ai_generated"] != false || j.Metadata["best_time"] != true {
		t.Fatalf("job %+v", j)
	}
}

func TestOSTurnPublishNoAccountsAndPPB(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	tenant, version := seedVideo(t, db)
	user := uuid.New()
	h := &Handler{Store: &EntStore{DB: db}, Logger: zap.NewNop(), WhatsApp: WhatsAppPublishDeps{
		IsApprover: func(context.Context, uuid.UUID, uuid.UUID) bool { return true },
		Approve:    func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) error { return ErrPPBHold },
	}}
	body := map[string]any{"tenant_id": tenant.String(), "version_id": version.String(), "user_id": user.String(), "run_id": "r"}
	if code, out := osTurnPublish(h, body); code != http.StatusConflict || out["code"] != "needs_review" {
		t.Fatalf("ppb: %d %v", code, out)
	}
	if n := db.PublishJob.Query().Where(publishjob.TenantIDEQ(tenant)).CountX(ctx); n != 0 {
		t.Fatalf("a held ad posts nothing: %d", n)
	}
	db.ConnectedAccount.Update().Where(connectedaccount.TenantIDEQ(tenant)).SetStatus("revoked").ExecX(ctx)
	h.WhatsApp.Approve = func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) error { return errors.New("unexpected") }
	if code, out := osTurnPublish(h, body); code != http.StatusConflict || out["code"] != "no_accounts" {
		t.Fatalf("no accounts: %d %v", code, out)
	}
	if code, _ := osTurnPublish(h, map[string]any{"tenant_id": "x"}); code != http.StatusBadRequest {
		t.Fatalf("bad body: %d", code)
	}
}
