package creative

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/notifications"
)

type fakeStatuses struct {
	calls int
	err   error
	st    map[string]notifications.TemplateStatus
}

func (f *fakeStatuses) TemplateStatuses(context.Context, string, []string) (map[string]notifications.TemplateStatus, error) {
	f.calls++
	return f.st, f.err
}

func TestWhatsAppTemplateConfigFromEnv(t *testing.T) {
	cfg, err := WhatsAppTemplateConfigFromEnv(func(string) string { return "" })
	if err != nil || cfg != DefaultWhatsAppTemplateConfig() || cfg.Name(TemplateDailyBrief) != "mf_daily_brief_v1_btn" ||
		cfg.Name(TemplateOTP) != "auth_otp" || cfg.Name("nope") != "" {
		t.Fatalf("defaults: %+v %v", cfg, err)
	}
	env := map[string]string{"MF_WA_TPL_VIDEO_READY": "mf_video_ready_v2_btn", "MF_WA_TPL_DAILY_BRIEF": "Bad Name!", "MF_WA_TPL_STATUS_TTL": "5m"}
	cfg, err = WhatsAppTemplateConfigFromEnv(func(k string) string { return env[k] })
	if err == nil || cfg.VideoReady != "mf_video_ready_v2_btn" || cfg.DailyBrief != "mf_daily_brief_v1_btn" || cfg.StatusTTL != 5*time.Minute {
		t.Fatalf("overrides: %+v %v", cfg, err)
	}
}

// Only an APPROVED template may go; the status is cached for the TTL, and a failed lookup sends
// nothing until notifications-api answers.
func TestTemplateGate(t *testing.T) {
	now := time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC)
	src := &fakeStatuses{st: map[string]notifications.TemplateStatus{
		"mf_video_ready_v1_btn": {Name: "mf_video_ready_v1_btn", Status: "APPROVED"},
		"mf_daily_brief_v1_btn": {Name: "mf_daily_brief_v1_btn", Status: "PENDING"},
	}}
	g := &templateGate{cfg: DefaultWhatsAppTemplateConfig(), src: src, now: func() time.Time { return now }}
	ctx, tid := context.Background(), uuid.New()
	if !g.approvedName(ctx, tid, "mf_video_ready_v1_btn") || g.approvedName(ctx, tid, "mf_daily_brief_v1_btn") ||
		g.approvedName(ctx, tid, "mf_publish_result_v1_btn") {
		t.Fatal("only APPROVED may go")
	}
	if src.calls != 1 {
		t.Fatalf("cached: %d calls", src.calls)
	}
	now = now.Add(16 * time.Minute)
	src.st["mf_daily_brief_v1_btn"] = notifications.TemplateStatus{Status: "APPROVED"}
	if !g.approvedName(ctx, tid, "mf_daily_brief_v1_btn") || src.calls != 2 {
		t.Fatalf("refreshed after the TTL: %d calls", src.calls)
	}

	down := &templateGate{cfg: DefaultWhatsAppTemplateConfig(), src: &fakeStatuses{err: errors.New("down")}, now: time.Now}
	if down.approvedName(ctx, tid, "mf_video_ready_v1_btn") {
		t.Fatal("an unknown status must not send")
	}
	var none *templateGate
	if none.approvedName(ctx, tid, "mf_video_ready_v1_btn") {
		t.Fatal("no gate, no template")
	}
}

// Without WithTemplates nothing is sent as a template, except the one-time code template, which
// is the platform's own and always approved.
func TestPlatformWhatsAppTemplateApproved(t *testing.T) {
	p := NewPlatformWhatsApp(nil, nil, "codevertex", "test")
	if p.TemplateApproved(context.Background(), TemplateVideoReady) {
		t.Fatal("no templates configured")
	}
	if sent, err := p.SendTemplate(context.Background(), "+254700000001", TemplateVideoReady, nil, "", "x", "k"); sent || err != nil {
		t.Fatalf("send without notifications: %v %v", sent, err)
	}
	p.WithTemplates(WhatsAppTemplateConfig{}, nil)
	if !p.TemplateApproved(context.Background(), TemplateOTP) {
		t.Fatal("auth_otp is always approved")
	}
	if p.tpl.cfg.VideoReady != "mf_video_ready_v1_btn" || p.tpl.cfg.StatusTTL != 15*time.Minute {
		t.Fatalf("defaults filled: %+v", p.tpl.cfg)
	}
}
