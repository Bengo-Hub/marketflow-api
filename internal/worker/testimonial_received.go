package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/bengobox/marketflow-api/internal/modules/rbac"
	"github.com/bengobox/marketflow-api/internal/modules/testimonials"
	"github.com/bengobox/marketflow-api/internal/notifications"
)

// A customer's testimonial waits for review: the tenant's MarketFlow admins get an email, and the
// business's WhatsApp number gets the UTILITY template mf_testimonial_received_v1_btn from the
// platform number once Meta approves it (notifications-api docs/whatsapp-template-policy.md). The
// button opens Settings, Testimonials at the testimonial. Website finds are told once per sweep
// (website_testimonials.go), not one message each.

// DefaultTestimonialTemplate is the WhatsApp template name (MF_WA_TPL_TESTIMONIAL_RECEIVED
// replaces it with a _v2 after a refusal).
const DefaultTestimonialTemplate = "mf_testimonial_received_v1_btn"

// testimonialReviewPath is the review page under the app (button suffix and email link).
func testimonialReviewPath(slug string, id uuid.UUID) string {
	return slug + "/settings/testimonials?review=" + id.String()
}

// TemplateGate sends one platform WhatsApp template only while Meta shows it APPROVED, asking
// notifications-api at most every ttl.
type TemplateGate struct {
	notif        *notifications.Client
	platformSlug string
	db           *ent.Client
	ttl          time.Duration

	mu       sync.Mutex
	platform uuid.UUID
	status   map[string]bool
	checked  map[string]time.Time
}

func NewTemplateGate(db *ent.Client, notif *notifications.Client, platformSlug string) *TemplateGate {
	return &TemplateGate{notif: notif, platformSlug: platformSlug, db: db, ttl: 15 * time.Minute,
		status: map[string]bool{}, checked: map[string]time.Time{}}
}

func (g *TemplateGate) platformID(ctx context.Context) (uuid.UUID, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.platform != uuid.Nil {
		return g.platform, nil
	}
	t, err := g.db.Tenant.Query().Where(tenant.SlugEQ(g.platformSlug)).Only(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	g.platform = t.ID
	return t.ID, nil
}

func (g *TemplateGate) approved(ctx context.Context, platform uuid.UUID, name string) bool {
	g.mu.Lock()
	if at, ok := g.checked[name]; ok && time.Since(at) < g.ttl {
		ok := g.status[name]
		g.mu.Unlock()
		return ok
	}
	g.mu.Unlock()
	m, err := g.notif.TemplateStatuses(ctx, platform.String(), []string{name})
	ok := err == nil && strings.EqualFold(m[name].Status, "APPROVED")
	g.mu.Lock()
	g.status[name], g.checked[name] = ok, time.Now()
	g.mu.Unlock()
	return ok
}

// Send sends the template to a number; false (nil error) when it is not approved yet.
func (g *TemplateGate) Send(ctx context.Context, to, name string, params []string, button, text, key, kind string) (bool, error) {
	if g == nil || g.notif == nil || strings.TrimSpace(to) == "" {
		return false, nil
	}
	platform, err := g.platformID(ctx)
	if err != nil || !g.approved(ctx, platform, name) {
		return false, err
	}
	err = g.notif.Send(ctx, platform.String(), notifications.Message{
		Channel: "whatsapp", Recipient: "+" + strings.TrimPrefix(strings.TrimSpace(to), "+"), Body: text,
		Metadata:       map[string]string{"source": "marketflow-worker", "kind": kind, "preference_key": "marketflow/" + kind + "_whatsapp"},
		IdempotencyKey: key, PlatformDialCode: "254",
		Template: &notifications.WhatsAppTemplate{Name: name, Params: params, ButtonParam: button},
	})
	return err == nil, err
}

// TestimonialReceivedHandler tells a tenant's admins that a customer sent a testimonial.
type TestimonialReceivedHandler struct {
	db       *ent.Client
	notif    *notifications.Client
	gate     *TemplateGate
	appURL   string
	template string
	logger   *zap.Logger
}

func NewTestimonialReceivedHandler(db *ent.Client, notif *notifications.Client, platformSlug, appURL string, logger *zap.Logger) *TestimonialReceivedHandler {
	tpl := strings.TrimSpace(os.Getenv("MF_WA_TPL_TESTIMONIAL_RECEIVED"))
	if tpl == "" {
		tpl = DefaultTestimonialTemplate
	}
	return &TestimonialReceivedHandler{db: db, notif: notif, gate: NewTemplateGate(db, notif, platformSlug),
		appURL: strings.TrimRight(appURL, "/"), template: tpl, logger: logger.Named("testimonial-received")}
}

func (*TestimonialReceivedHandler) Subject() string { return testimonials.TestimonialSubmittedTopic }
func (*TestimonialReceivedHandler) Durable() string { return "marketflow-testimonial-received" }

type testimonialSubmitted struct {
	TenantID      string `json:"tenant_id"`
	TestimonialID string `json:"testimonial_id"`
	Source        string `json:"source"`
}

func (h *TestimonialReceivedHandler) Handle(ctx context.Context, msg jetstream.Msg) error {
	var ev testimonialSubmitted
	if err := json.Unmarshal(eventBody(msg.Data()), &ev); err != nil {
		return dropPoison(h.logger, msg, "decode "+testimonials.TestimonialSubmittedTopic, err)
	}
	tid, err1 := uuid.Parse(ev.TenantID)
	id, err2 := uuid.Parse(ev.TestimonialID)
	if err1 != nil || err2 != nil {
		return dropPoison(h.logger, msg, "decode "+testimonials.TestimonialSubmittedTopic, fmt.Errorf("ids: %v %v", err1, err2))
	}
	if ev.Source != testimonials.SourceForm || h.db == nil || h.notif == nil {
		return nil
	}
	return h.notify(ctx, tid, id)
}

func (h *TestimonialReceivedHandler) notify(ctx context.Context, tid, id uuid.UUID) error {
	t, err := h.db.Testimonial.Get(ctx, id)
	if err != nil || t.TenantID != tid || t.Status != testimonials.StatusPending {
		return nil
	}
	ten, err := h.db.Tenant.Get(ctx, tid)
	if err != nil {
		return nil
	}
	path := testimonialReviewPath(ten.Slug, id)
	subject, body := testimonialEmail(t.AuthorName, t.Rating, ten.Name, h.appURL+"/"+path)
	emails, _ := rbac.TenantAdminEmails(ctx, h.db, tid, rbac.NoticeRoles)
	for _, e := range emails {
		if err := h.notif.Send(ctx, tid.String(), notifications.Message{
			Channel: "email", Recipient: e, Subject: subject, Body: body,
			Metadata:       map[string]string{"source": "marketflow", "kind": "testimonial_received"},
			IdempotencyKey: "testimonial-received-" + id.String() + "-" + e,
		}); err != nil {
			h.logger.Warn("testimonial email not sent", zap.String("tenant", tid.String()), zap.Error(err))
		}
	}
	if ten.WhatsappNumber != nil {
		// Body: "Hi {{1}},\n\n{{2}} sent your business a new testimonial through your testimonial
		// link. It is not shown anywhere until you approve it.\n\nOpen it to approve or decline."
		params := []string{ten.Name, firstWord(t.AuthorName)}
		text := fmt.Sprintf("%s sent a new testimonial for %s. It waits for your review before it is shown anywhere.", firstWord(t.AuthorName), ten.Name)
		if _, err := h.gate.Send(ctx, *ten.WhatsappNumber, h.template, params, path, text, "testimonial-received-wa-"+id.String(), "testimonial_received"); err != nil {
			h.logger.Warn("testimonial WhatsApp not sent", zap.String("tenant", tid.String()), zap.Error(err))
		}
	}
	return nil
}

func firstWord(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return "A customer"
}

// testimonialEmail is the admins' email: who wrote, the rating, and the review link. The words are
// in the app, not the email, so nothing unreviewed is forwarded around.
func testimonialEmail(author string, rating int, business, link string) (string, string) {
	subject := "New testimonial from " + author
	stars := ""
	if rating > 0 {
		stars = fmt.Sprintf(" and rated %s %d out of 5", business, rating)
	}
	body := fmt.Sprintf("%s sent a testimonial through your testimonial link%s.\n\n"+
		"It is not shown anywhere until you approve it. Review it here: %s\n\n"+
		"Approved testimonials appear on your public page and can be used in testimonial ads, word for word.",
		author, stars, link)
	return subject, body
}
