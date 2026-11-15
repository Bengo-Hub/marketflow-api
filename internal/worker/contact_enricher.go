package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	sharedcache "github.com/Bengo-Hub/cache"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/bengobox/marketflow-api/internal/modules/contacts"
	"github.com/bengobox/marketflow-api/internal/modules/enrichment"
)

// The contact enricher (Sprint 10) keeps Contact.metadata.enrichment filled from the business's
// own records: the lead, the timeline, purchases and payments, deals, the linked company account
// and, when the owner turns it on, that company's public home page. It never searches for a
// person on the web, calls no paid API and uses no AI, so it charges no credits. It runs:
//   - on crm.contact.created and crm.contact.updated, for that contact at once;
//   - daily from 02:00 tenant time, for contacts never enriched (bulk imports emit no event, and
//     contacts from before the enricher) and profiles older than enricherRefreshAge, at most
//     enricherDailyCap per tenant.
// The owner switches it off in Marketing OS settings (marketingos.ContactEnrichmentKey).

// ContactEnricherHandler refreshes one contact when a contact event arrives.
type ContactEnricherHandler struct {
	subject string
	durable string
	enr     *enrichment.Enricher
	logger  *zap.Logger
}

// NewContactEnricherHandlers returns the created and updated handlers.
func NewContactEnricherHandlers(db *ent.Client, logger *zap.Logger) []Handler {
	enr := enrichment.New(db)
	log := logger.Named("agent.contact-enricher")
	return []Handler{
		&ContactEnricherHandler{subject: contacts.ContactCreatedTopic, durable: "marketflow-worker-agent-contact-enricher-created", enr: enr, logger: log},
		&ContactEnricherHandler{subject: contacts.ContactUpdatedTopic, durable: "marketflow-worker-agent-contact-enricher-updated", enr: enr, logger: log},
	}
}

func (h *ContactEnricherHandler) Subject() string { return h.subject }
func (h *ContactEnricherHandler) Durable() string { return h.durable }

// Handle rebuilds the profile. A redelivery builds the same profile again and overwrites one
// metadata key, so it needs no dedupe record.
func (h *ContactEnricherHandler) Handle(ctx context.Context, msg jetstream.Msg) error {
	var ev struct {
		TenantID  string `json:"tenant_id"`
		ContactID string `json:"contact_id"`
	}
	if err := json.Unmarshal(eventBody(msg.Data()), &ev); err != nil {
		return dropPoison(h.logger, msg, "contact enricher: decode", err)
	}
	tenantID, err := uuid.Parse(ev.TenantID)
	if err != nil {
		return dropPoison(h.logger, msg, "contact enricher: bad tenant_id", err)
	}
	contactID, err := uuid.Parse(ev.ContactID)
	if err != nil {
		return dropPoison(h.logger, msg, "contact enricher: bad contact_id", err)
	}
	cctx, cancel := context.WithTimeout(ctx, enricherContactBudget)
	defer cancel()
	if _, err := h.enr.Enrich(cctx, tenantID, contactID); err != nil {
		// A DB blip: the registry retries with spaced delays.
		return fmt.Errorf("contact enricher: %w", err)
	}
	return nil
}

const (
	// enricherRunAfter is the tenant-local time the daily sweep starts from.
	enricherRunAfter = 2 * time.Hour
	// enricherRefreshAge is how old a profile gets before the sweep rebuilds it.
	enricherRefreshAge = 7 * 24 * time.Hour
	// enricherDailyCap bounds one tenant's sweep; a large backlog drains over the next days,
	// never-enriched contacts first.
	enricherDailyCap = 1000
	// enricherPage is how many contacts one query reads.
	enricherPage = 100
	// enricherContactBudget bounds one contact: a handful of indexed queries and, with the
	// website switch on, one cached page read.
	enricherContactBudget = 20 * time.Second
	// enricherFailLimit stops a tenant's sweep after this many failures in a row (the database
	// is struggling); the next day picks up where it stopped.
	enricherFailLimit = 5
)

// ContactEnricherSweep is the daily pass over each tenant's contacts.
type ContactEnricherSweep struct {
	db     *ent.Client
	enr    *enrichment.Enricher
	every  time.Duration
	claim  claimFn
	now    func() time.Time
	logger *zap.Logger
}

// NewContactEnricherSweep builds the sweep, claiming each tenant's day through sharedcache.
func NewContactEnricherSweep(db *ent.Client, logger *zap.Logger) *ContactEnricherSweep {
	return &ContactEnricherSweep{db: db, enr: enrichment.New(db), every: 15 * time.Minute,
		claim: sharedcache.ClaimOnce, now: time.Now, logger: logger.Named("agent.contact-enricher.sweep")}
}

// Run ticks every 15 minutes until ctx ends.
func (j *ContactEnricherSweep) Run(ctx context.Context) {
	t := time.NewTicker(j.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := j.tick(ctx); err != nil && ctx.Err() == nil {
				j.logger.Warn("contact enricher sweep failed", zap.Error(err))
			}
		}
	}
}

// tick walks the tenants in keyset pages and sweeps each one that is due today.
func (j *ContactEnricherSweep) tick(ctx context.Context) error {
	var after uuid.UUID
	for {
		q := j.db.Tenant.Query().Order(ent.Asc(tenant.FieldID)).Limit(crmTenantPage)
		if after != uuid.Nil {
			q = q.Where(tenant.IDGT(after))
		}
		rows, err := q.All(ctx)
		if err != nil {
			return err
		}
		for _, t := range rows {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			n, err := j.forTenant(ctx, t)
			if err != nil {
				j.logger.Warn("contact enricher: tenant sweep stopped", zap.String("tenant", t.ID.String()), zap.Error(err))
			}
			if n > 0 {
				j.logger.Info("contact profiles refreshed", zap.String("tenant", t.ID.String()), zap.Int("contacts", n))
			}
		}
		if len(rows) < crmTenantPage {
			return nil
		}
		after = rows[len(rows)-1].ID
	}
}

// forTenant runs the tenant's sweep once per local day, from enricherRunAfter.
func (j *ContactEnricherSweep) forTenant(ctx context.Context, t *ent.Tenant) (int, error) {
	on, web := enrichment.Switches(t.Metadata)
	if !on {
		return 0, nil
	}
	now := j.now().In(zoneOf(t))
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	if now.Sub(dayStart) < enricherRunAfter {
		return 0, nil
	}
	// Claim before any work, so replicas and later ticks skip the tenant for the day; a crash
	// after the claim leaves the rest to tomorrow, which is harmless for derived data.
	if !j.claim(ctx, "marketflow:contact-enricher:"+t.ID.String()+":"+dayStart.Format("2006-01-02"), 36*time.Hour) {
		return 0, nil
	}
	preds, order := enrichment.Stale(t.ID, j.now().Add(-enricherRefreshAge))
	done, fails := 0, 0
	for seen := 0; seen < enricherDailyCap; {
		// Each pass reads the stalest page again: contacts refreshed in the previous pass have
		// left the stale set, and ones that failed or were skipped are counted in seen, so the
		// loop always ends.
		limit := min(enricherPage, enricherDailyCap-seen)
		batch, err := j.db.Contact.Query().Where(preds...).Order(order).Limit(limit).All(ctx)
		if err != nil {
			return done, err
		}
		if len(batch) == 0 {
			return done, nil
		}
		progressed := false
		for _, c := range batch {
			seen++
			cctx, cancel := context.WithTimeout(ctx, enricherContactBudget)
			ok, err := j.enr.EnrichContact(cctx, c, web)
			cancel()
			if err != nil {
				fails++
				j.logger.Warn("contact enricher: contact failed", zap.String("contact", c.ID.String()), zap.Error(err))
				if fails >= enricherFailLimit {
					return done, fmt.Errorf("%d contacts failed in a row", fails)
				}
				continue
			}
			fails = 0
			if ok {
				done++
				progressed = true
			}
		}
		// Nothing in the page could be written (each was edited meanwhile, or failed): the next
		// query would return the same rows, so stop and leave them for tomorrow.
		if !progressed || len(batch) < limit {
			return done, nil
		}
	}
	return done, nil
}

// zoneOf is the time zone of a loaded tenant, Africa/Nairobi when unset or unknown (tenantLocation
// does the same from an id).
func zoneOf(t *ent.Tenant) *time.Location {
	if t.Timezone != nil && *t.Timezone != "" {
		if loc, err := time.LoadLocation(*t.Timezone); err == nil {
			return loc
		}
	}
	if loc, err := time.LoadLocation("Africa/Nairobi"); err == nil {
		return loc
	}
	return time.FixedZone("EAT", 3*3600)
}
