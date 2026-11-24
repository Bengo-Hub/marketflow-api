package worker

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	sharedcache "github.com/Bengo-Hub/cache"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/bengobox/marketflow-api/internal/modules/rbac"
	"github.com/bengobox/marketflow-api/internal/modules/testimonials"
	"github.com/bengobox/marketflow-api/internal/notifications"
	"github.com/bengobox/marketflow-api/internal/platform/publicweb"
)

// WebsiteTestimonialSweep reads each tenant's website once a week for the testimonials it shows
// (testimonials.ImportFromWebsite) and stores the new ones pending review. The admins get one email
// per sweep that found something; nothing is published until they approve it.
type WebsiteTestimonialSweep struct {
	db     *ent.Client
	notif  *notifications.Client
	appURL string
	hc     *http.Client
	logger *zap.Logger
	claim  func(ctx context.Context, key string, ttl time.Duration) bool
	now    func() time.Time
}

func NewWebsiteTestimonialSweep(db *ent.Client, notif *notifications.Client, appURL string, logger *zap.Logger) *WebsiteTestimonialSweep {
	return &WebsiteTestimonialSweep{db: db, notif: notif, appURL: strings.TrimRight(appURL, "/"), hc: publicweb.Client(10*time.Second, 3),
		logger: logger.Named("website-testimonials"), claim: sharedcache.ClaimOnce, now: time.Now}
}

// websiteSweepEvery is how often one tenant's site is read: testimonials on a site change slowly.
const websiteSweepEvery = 7 * 24 * time.Hour

// Run checks every six hours; each tenant is read at most once per ISO week (the claim key).
func (s *WebsiteTestimonialSweep) Run(ctx context.Context) {
	t := time.NewTicker(6 * time.Hour)
	defer t.Stop()
	for {
		s.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *WebsiteTestimonialSweep) tick(ctx context.Context) {
	year, week := s.now().UTC().ISOWeek()
	var after uuid.UUID
	for ctx.Err() == nil {
		q := s.db.Tenant.Query().
			Where(tenant.StatusEQ("active"), func(sel *entsql.Selector) {
				sel.Where(entsql.ExprP("COALESCE(" + sel.C(tenant.FieldMetadata) + "->'socials'->>'website', '') <> ''"))
			}).
			Order(ent.Asc(tenant.FieldID)).Limit(200)
		if after != uuid.Nil {
			q = q.Where(tenant.IDGT(after))
		}
		rows, err := q.All(ctx)
		if err != nil {
			s.logger.Warn("load tenants", zap.Error(err))
			return
		}
		for _, t := range rows {
			if s.claim(ctx, fmt.Sprintf("marketflow:website-testimonials:%s:%d-%d", t.ID, year, week), websiteSweepEvery+24*time.Hour) {
				s.read(ctx, t)
			}
		}
		if len(rows) < 200 {
			return
		}
		after = rows[len(rows)-1].ID
	}
}

func (s *WebsiteTestimonialSweep) read(ctx context.Context, t *ent.Tenant) {
	socials, _ := t.Metadata["socials"].(map[string]any)
	site, _ := socials["website"].(string)
	rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	res, err := testimonials.ImportFromWebsite(rctx, s.db, s.hc, t.ID, site)
	cancel()
	if err != nil {
		s.logger.Info("website not read", zap.String("tenant", t.ID.String()), zap.Error(err))
		return
	}
	if res.Stored == 0 || s.notif == nil {
		return
	}
	emails, _ := rbac.TenantAdminEmails(ctx, s.db, t.ID, rbac.NoticeRoles)
	link := s.appURL + "/" + t.Slug + "/settings/testimonials?status=pending"
	subject, body := websiteTestimonialsEmail(res.Stored, site, link)
	for _, e := range emails {
		if err := s.notif.Send(ctx, t.ID.String(), notifications.Message{
			Channel: "email", Recipient: e, Subject: subject, Body: body,
			Metadata:       map[string]string{"source": "marketflow", "kind": "website_testimonials_found"},
			IdempotencyKey: fmt.Sprintf("website-testimonials-%s-%s-%s", t.ID, s.now().UTC().Format("2006-01-02"), e),
		}); err != nil {
			s.logger.Warn("website testimonials email not sent", zap.String("tenant", t.ID.String()), zap.Error(err))
		}
	}
}

func websiteTestimonialsEmail(n int, site, link string) (string, string) {
	what := "testimonial"
	if n > 1 {
		what = "testimonials"
	}
	subject := fmt.Sprintf("%d %s found on your website", n, what)
	body := fmt.Sprintf("We found %d %s on %s that MarketFlow did not have yet. They are saved word for word, "+
		"credited as your website credits them, and are not shown anywhere until you approve them.\n\nReview them here: %s\n\n"+
		"Approved testimonials appear on your public page and can be used in testimonial ads.", n, what, site, link)
	return subject, body
}
