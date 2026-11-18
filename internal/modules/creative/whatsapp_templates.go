package creative

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent/creativebrief"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/bengobox/marketflow-api/internal/notifications"
)

// WhatsApp utility templates for outside the 24-hour window (Sprint 17). Meta takes a free-form
// message only inside the window the person's last message opened; outside it only an approved
// template goes. The owner submits the templates in WhatsApp Manager (notifications-api
// docs/whatsapp-template-policy.md, section 5); MarketFlow sends one only while Meta shows it
// APPROVED on the platform number's account, and otherwise sends nothing on WhatsApp for that
// event (the Studio, the calendar and the email brief still show it).

// Template kinds, as marketflow-ai names them.
const (
	TemplateVideoReady    = "video_ready"
	TemplatePublishResult = "publish_result"
	TemplateDailyBrief    = "daily_brief"
	// TemplateWeeklySummary is the Monday summary (worker weekly_report.go).
	TemplateWeeklySummary = "weekly_summary"
	// TemplateIntegrationReconnect asks the business to reconnect an account whose token stopped
	// or is about to stop working (worker connected_token_health.go).
	TemplateIntegrationReconnect = "integration_reconnect"
	// TemplateOTP is the platform's AUTHENTICATION template for one-time codes; it is always
	// approved on the platform number and is never gated.
	TemplateOTP = "auth_otp"
)

// WhatsAppTemplateConfig names the templates and how long their review status is cached.
type WhatsAppTemplateConfig struct {
	VideoReady    string
	PublishResult string
	DailyBrief    string
	// WeeklySummary and IntegrationReconnect: sent, like the others, only while Meta shows them
	// APPROVED, so nothing goes on WhatsApp outside the window until the owner submits them.
	WeeklySummary        string
	IntegrationReconnect string
	// StatusTTL is how long Meta's review status is trusted before it is asked again.
	StatusTTL time.Duration
}

// DefaultWhatsAppTemplateConfig is the names in the template policy doc and 15 minutes.
func DefaultWhatsAppTemplateConfig() WhatsAppTemplateConfig {
	return WhatsAppTemplateConfig{VideoReady: "mf_video_ready_v1_btn", PublishResult: "mf_publish_result_v1_btn",
		DailyBrief: "mf_daily_brief_v1_btn", WeeklySummary: "mf_weekly_summary_v1_btn",
		IntegrationReconnect: "mf_integration_reconnect_v1_btn", StatusTTL: 15 * time.Minute}
}

// WhatsAppTemplateConfigFromEnv reads MF_WA_TPL_VIDEO_READY, MF_WA_TPL_PUBLISH_RESULT,
// MF_WA_TPL_DAILY_BRIEF (a template name, for a _v2 after a refusal) and MF_WA_TPL_STATUS_TTL
// (a Go duration). A bad value keeps its default and is listed in the error.
func WhatsAppTemplateConfigFromEnv(getenv func(string) string) (WhatsAppTemplateConfig, error) {
	cfg := DefaultWhatsAppTemplateConfig()
	var bad []string
	name := func(key string, dst *string) {
		v := strings.TrimSpace(getenv(key))
		if v == "" {
			return
		}
		if !validTemplateName(v) {
			bad = append(bad, key+" must be a lower-case template name")
			return
		}
		*dst = v
	}
	name("MF_WA_TPL_VIDEO_READY", &cfg.VideoReady)
	name("MF_WA_TPL_PUBLISH_RESULT", &cfg.PublishResult)
	name("MF_WA_TPL_DAILY_BRIEF", &cfg.DailyBrief)
	name("MF_WA_TPL_WEEKLY_SUMMARY", &cfg.WeeklySummary)
	name("MF_WA_TPL_INTEGRATION_RECONNECT", &cfg.IntegrationReconnect)
	if v := strings.TrimSpace(getenv("MF_WA_TPL_STATUS_TTL")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.StatusTTL = d
		} else {
			bad = append(bad, "MF_WA_TPL_STATUS_TTL must be a positive duration")
		}
	}
	if len(bad) > 0 {
		return cfg, fmt.Errorf("whatsapp templates: %s", strings.Join(bad, "; "))
	}
	return cfg, nil
}

// validTemplateName is Meta's rule: lower-case letters, digits and underscores, at most 512.
func validTemplateName(s string) bool {
	if s == "" || len(s) > 512 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return false
		}
	}
	return true
}

// Name is the template for a kind ("" for an unknown kind).
func (c WhatsAppTemplateConfig) Name(kind string) string {
	switch kind {
	case TemplateVideoReady:
		return c.VideoReady
	case TemplatePublishResult:
		return c.PublishResult
	case TemplateDailyBrief:
		return c.DailyBrief
	case TemplateWeeklySummary:
		return c.WeeklySummary
	case TemplateIntegrationReconnect:
		return c.IntegrationReconnect
	case TemplateOTP:
		return TemplateOTP
	}
	return ""
}

func (c WhatsAppTemplateConfig) gated() []string {
	return []string{c.VideoReady, c.PublishResult, c.DailyBrief, c.WeeklySummary, c.IntegrationReconnect}
}

// templateStatuses reads review statuses (notifications.Client.TemplateStatuses).
type templateStatuses interface {
	TemplateStatuses(ctx context.Context, tenantID string, names []string) (map[string]notifications.TemplateStatus, error)
}

// templateGate caches which gated templates Meta approved on the platform number's account.
type templateGate struct {
	cfg    WhatsAppTemplateConfig
	src    templateStatuses
	now    func() time.Time
	logger *zap.Logger

	mu       sync.Mutex
	approved map[string]bool
	at       time.Time
}

// approvedName reports whether the named template is APPROVED. A failed lookup keeps the last
// answer (none at first, so nothing goes until notifications-api answers).
func (g *templateGate) approvedName(ctx context.Context, tenantID uuid.UUID, name string) bool {
	if g == nil || g.src == nil || name == "" {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.approved == nil || g.now().Sub(g.at) >= g.cfg.StatusTTL {
		st, err := g.src.TemplateStatuses(ctx, tenantID.String(), g.cfg.gated())
		if err != nil {
			if g.logger != nil {
				g.logger.Warn("whatsapp template status unavailable", zap.Error(err))
			}
			g.at = g.now().Add(-g.cfg.StatusTTL + time.Minute) // ask again in a minute
			if g.approved == nil {
				g.approved = map[string]bool{}
			}
		} else {
			g.approved = map[string]bool{}
			for n, s := range st {
				g.approved[n] = strings.EqualFold(s.Status, "APPROVED")
			}
			g.at = g.now()
		}
	}
	return g.approved[name]
}

// WithTemplates turns on the utility templates for numbers whose window is closed.
func (p *PlatformWhatsApp) WithTemplates(cfg WhatsAppTemplateConfig, logger *zap.Logger) *PlatformWhatsApp {
	d := DefaultWhatsAppTemplateConfig()
	if cfg.StatusTTL <= 0 {
		cfg.StatusTTL = d.StatusTTL
	}
	if cfg.VideoReady == "" {
		cfg.VideoReady = d.VideoReady
	}
	if cfg.PublishResult == "" {
		cfg.PublishResult = d.PublishResult
	}
	if cfg.DailyBrief == "" {
		cfg.DailyBrief = d.DailyBrief
	}
	if cfg.WeeklySummary == "" {
		cfg.WeeklySummary = d.WeeklySummary
	}
	if cfg.IntegrationReconnect == "" {
		cfg.IntegrationReconnect = d.IntegrationReconnect
	}
	var src templateStatuses
	if p.notif != nil {
		src = p.notif
	}
	p.tpl = &templateGate{cfg: cfg, src: src, now: time.Now, logger: logger}
	return p
}

// TemplateApproved reports whether the template of a kind may be sent now.
func (p *PlatformWhatsApp) TemplateApproved(ctx context.Context, kind string) bool {
	if p == nil || p.tpl == nil {
		return false
	}
	if kind == TemplateOTP {
		return true
	}
	id, err := p.platformID(ctx)
	if err != nil {
		return false
	}
	return p.tpl.approvedName(ctx, id, p.tpl.cfg.Name(kind))
}

// SendTemplate sends the template of a kind with its parameters and button suffix, only when
// Meta approved it. sent is false (and err nil) when it is not approved, so the caller can fall
// back to another channel. text is what notifications-api records as the message body.
func (p *PlatformWhatsApp) SendTemplate(ctx context.Context, to, kind string, params []string, button, text, key string) (bool, error) {
	if p == nil || p.notif == nil || !p.TemplateApproved(ctx, kind) {
		return false, nil
	}
	id, err := p.platformID(ctx)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(text) == "" {
		text = kind
	}
	err = p.notif.Send(ctx, id.String(), notifications.Message{
		Channel:          "whatsapp",
		Recipient:        "+" + strings.TrimPrefix(to, "+"),
		Body:             text,
		Metadata:         map[string]string{"source": p.source, "kind": "whatsapp_assistant_" + kind},
		IdempotencyKey:   key,
		Template:         &notifications.WhatsAppTemplate{Name: p.tpl.cfg.Name(kind), Params: params, ButtonParam: button},
		PlatformDialCode: "254",
	})
	return err == nil, err
}

// SendOTP sends a one-time code by the platform's auth_otp template (the code is the body
// parameter and the copy button's value).
func (p *PlatformWhatsApp) SendOTP(ctx context.Context, to, code, key string) error {
	if p == nil || p.notif == nil {
		return nil
	}
	id, err := p.platformID(ctx)
	if err != nil {
		return err
	}
	return p.notif.Send(ctx, id.String(), notifications.Message{
		Channel:          "whatsapp",
		Recipient:        "+" + strings.TrimPrefix(to, "+"),
		Body:             "Your MarketFlow confirmation code.",
		Metadata:         map[string]string{"source": p.source, "kind": "whatsapp_assistant_otp"},
		IdempotencyKey:   key,
		Template:         &notifications.WhatsAppTemplate{Name: TemplateOTP, Params: []string{code}, ButtonParam: code},
		PlatformDialCode: "254",
	})
}

// videoReadyParams are mf_video_ready's parameters: the business and the ad's title.
func (p *PlatformWhatsApp) videoReadyParams(ctx context.Context, tenantID uuid.UUID, versionID string) []string {
	business, title := "your business", "your video ad"
	if p == nil || p.db == nil {
		return []string{business, title}
	}
	if t, err := p.db.Tenant.Get(ctx, tenantID); err == nil && strings.TrimSpace(t.Name) != "" {
		business = t.Name
	}
	vid, perr := uuid.Parse(versionID)
	if perr != nil {
		return []string{excerpt(business, 60), title}
	}
	if v, err := p.db.CreativeVersion.Query().Where(creativeversion.IDEQ(vid), creativeversion.TenantIDEQ(tenantID)).Only(ctx); err == nil {
		if b, err := p.db.CreativeBrief.Query().Where(creativebrief.IDEQ(v.BriefID), creativebrief.TenantIDEQ(tenantID)).Only(ctx); err == nil && strings.TrimSpace(b.Title) != "" {
			title = b.Title
		}
	}
	return []string{excerpt(business, 60), excerpt(title, 60)}
}
