package enrichment

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqljson"
	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/account"
	"github.com/bengobox/marketflow-api/internal/ent/activity"
	"github.com/bengobox/marketflow-api/internal/ent/contact"
	"github.com/bengobox/marketflow-api/internal/ent/deal"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/ent/predicate"
	"github.com/bengobox/marketflow-api/internal/modules/marketingos"
)

// Enricher builds and stores contact profiles.
type Enricher struct {
	db *ent.Client
	// site reads a company's public home page summary (marketingos.SiteSummary).
	site func(ctx context.Context, link string) (string, error)
	now  func() time.Time
}

// New returns an Enricher that reads company websites through marketingos.SiteSummary.
func New(db *ent.Client) *Enricher {
	return &Enricher{db: db, site: marketingos.SiteSummary, now: time.Now}
}

// Switches reads the tenant's enricher switches (marketingos owns the keys).
func Switches(meta map[string]any) (on, web bool) { return marketingos.ContactEnrichmentSwitches(meta) }

// Erased reports a contact stripped by a privacy erasure; the enricher never writes to one.
func Erased(c *ent.Contact) bool {
	_, ok := c.Metadata["erased_at"]
	return ok
}

// NotErased is the SQL form of !Erased.
func NotErased() predicate.Contact {
	return func(s *sql.Selector) {
		s.Where(sql.Not(sqljson.HasKey(contact.FieldMetadata, sqljson.Path("erased_at"))))
	}
}

// Enrich refreshes one contact when the tenant's switch is on. It answers false, with no error,
// when nothing was written: the switch is off, the contact is gone or erased, or someone changed
// the contact while the profile was built (the next event or sweep picks it up).
func (e *Enricher) Enrich(ctx context.Context, tenantID, contactID uuid.UUID) (bool, error) {
	t, err := e.db.Tenant.Get(ctx, tenantID)
	if ent.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load tenant: %w", err)
	}
	on, web := Switches(t.Metadata)
	if !on {
		return false, nil
	}
	c, err := e.db.Contact.Query().Where(contact.ID(contactID), contact.TenantID(tenantID)).Only(ctx)
	if ent.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load contact: %w", err)
	}
	return e.EnrichContact(ctx, c, web)
}

// EnrichContact builds and stores the profile of a contact the caller already loaded.
func (e *Enricher) EnrichContact(ctx context.Context, c *ent.Contact, web bool) (bool, error) {
	if Erased(c) {
		return false, nil
	}
	p, err := e.Build(ctx, c, web)
	if err != nil {
		return false, err
	}
	return e.save(ctx, c, p)
}

// Build reads the contact's own records and returns its profile. Nothing is written.
func (e *Enricher) Build(ctx context.Context, c *ent.Contact, web bool) (*Profile, error) {
	now := e.now().UTC()
	p := &Profile{Version: Version, RefreshedAt: stamp(now), Sources: []string{SourceCRM}}

	if c.LeadID != nil {
		l, err := e.db.Lead.Query().Where(lead.ID(*c.LeadID), lead.TenantID(c.TenantID)).Only(ctx)
		switch {
		case err == nil:
			p.Origin = &Origin{LeadSource: l.Source, UTMSource: deref(l.UtmSource), UTMMedium: deref(l.UtmMedium),
				UTMCampaign: deref(l.UtmCampaign), Interest: deref(l.Interest), FirstSeenAt: stamp(l.CreatedAt)}
		case !ent.IsNotFound(err):
			return nil, fmt.Errorf("load lead: %w", err)
		}
	}
	if err := e.timeline(ctx, c, now, p); err != nil {
		return nil, err
	}
	if err := e.deals(ctx, c, p); err != nil {
		return nil, err
	}
	if c.Email != nil {
		if em := classifyEmail(*c.Email); em != nil {
			p.Email = em
			p.Sources = addSource(p.Sources, SourceEmailDomain)
		}
	}
	if err := e.company(ctx, c, web, now, p); err != nil {
		return nil, err
	}
	p.Segment = segmentFor(p, now)
	return p, nil
}

// onContact limits activities to the contact's own timeline.
func onContact(c *ent.Contact) []predicate.Activity {
	return []predicate.Activity{activity.TenantID(c.TenantID), activity.EntityType("contact"), activity.EntityID(c.ID)}
}

// byPeople leaves out what the agents wrote, so the enricher never counts its own kind.
func byPeople() predicate.Activity {
	return activity.And(activity.ActivityTypeNEQ("suggestion"), activity.Or(activity.SourceIsNil(), activity.SourceNEQ("agent")))
}

func (e *Enricher) timeline(ctx context.Context, c *ent.Contact, now time.Time, p *Profile) error {
	since := now.Add(-90 * 24 * time.Hour)
	n, err := e.db.Activity.Query().Where(append(onContact(c), byPeople(), activity.PerformedAtGTE(since))...).Count(ctx)
	if err != nil {
		return fmt.Errorf("count activities: %w", err)
	}
	p.Engagement.Activities90d = n
	last, err := e.db.Activity.Query().Where(append(onContact(c), byPeople())...).
		Order(ent.Desc(activity.FieldPerformedAt)).First(ctx)
	if err != nil && !ent.IsNotFound(err) {
		return fmt.Errorf("last activity: %w", err)
	}
	if last != nil {
		p.Engagement.LastActivityAt, p.Engagement.LastActivityType = stamp(last.PerformedAt), last.ActivityType
	}

	orders, err := e.db.Activity.Query().Where(append(onContact(c), activity.ActivityType("order"))...).Count(ctx)
	if err != nil {
		return fmt.Errorf("count orders: %w", err)
	}
	payments, err := e.db.Activity.Query().Where(append(onContact(c), activity.ActivityType("payment"))...).Count(ctx)
	if err != nil {
		return fmt.Errorf("count payments: %w", err)
	}
	// total_orders and last_order_at on the contact also hold history from before the
	// timeline consumers (FLP and POS backfills), so the larger figure wins.
	p.Purchases.Orders, p.Purchases.Payments = max(orders, c.TotalOrders), payments
	var lastBuy time.Time
	if c.LastOrderAt != nil {
		lastBuy = *c.LastOrderAt
	}
	buy, err := e.db.Activity.Query().Where(append(onContact(c), activity.ActivityTypeIn("order", "payment"))...).
		Order(ent.Desc(activity.FieldPerformedAt)).First(ctx)
	if err != nil && !ent.IsNotFound(err) {
		return fmt.Errorf("last purchase: %w", err)
	}
	if buy != nil && buy.PerformedAt.After(lastBuy) {
		lastBuy = buy.PerformedAt
	}
	if !lastBuy.IsZero() {
		p.Purchases.LastPurchaseAt = stamp(lastBuy)
	}
	return nil
}

// maxDeals bounds the deals read for one contact.
const maxDeals = 200

func (e *Enricher) deals(ctx context.Context, c *ent.Contact, p *Profile) error {
	rows, err := e.db.Deal.Query().
		Where(deal.TenantID(c.TenantID), deal.ContactIDEQ(c.ID), deal.StatusIn("open", "won")).
		Order(ent.Desc(deal.FieldCreatedAt)).Limit(maxDeals).All(ctx)
	if err != nil {
		return fmt.Errorf("load deals: %w", err)
	}
	for _, d := range rows {
		v := 0.0
		if d.Value != nil {
			v = *d.Value
		}
		if d.Status == "open" {
			p.Deals.Open++
			p.Deals.OpenValue += v
		} else {
			p.Deals.Won++
			p.Deals.WonValue += v
		}
		if p.Deals.Currency == "" {
			p.Deals.Currency = d.Currency
		}
	}
	return nil
}

// company fills the company block from the linked account, or suggests the one account whose
// website is on the contact's email domain. With web on, it adds that account's home page summary.
func (e *Enricher) company(ctx context.Context, c *ent.Contact, web bool, now time.Time, p *Profile) error {
	var acc *ent.Account
	suggested := false
	if c.AccountID != nil {
		a, err := e.db.Account.Query().Where(account.ID(*c.AccountID), account.TenantID(c.TenantID)).Only(ctx)
		if err != nil && !ent.IsNotFound(err) {
			return fmt.Errorf("load account: %w", err)
		}
		acc = a
	} else if p.Email != nil && p.Email.Domain != "" {
		rows, err := e.db.Account.Query().
			Where(account.TenantID(c.TenantID), account.WebsiteContainsFold(p.Email.Domain)).Limit(5).All(ctx)
		if err != nil {
			return fmt.Errorf("match account: %w", err)
		}
		var match []*ent.Account
		for _, a := range rows {
			if hostOf(deref(a.Website)) == p.Email.Domain {
				match = append(match, a)
			}
		}
		if len(match) == 1 {
			acc, suggested = match[0], true
		}
	}
	if acc == nil {
		return nil
	}
	co := &Company{Name: acc.Name, Industry: deref(acc.Industry), Website: deref(acc.Website)}
	if suggested {
		co.SuggestedAccountID = acc.ID.String()
	} else {
		co.AccountID = acc.ID.String()
	}
	p.Sources = addSource(p.Sources, SourceAccount)
	if web && co.Website != "" && e.site != nil {
		// The site client has its own short timeouts; this bounds the whole read.
		sctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		summary, err := e.site(sctx, co.Website)
		cancel()
		if err == nil && summary != "" {
			co.Summary, co.SummaryReadAt = summary, stamp(now)
		} else {
			keepSummary(co, previous(c))
		}
		if co.Summary != "" {
			p.Sources = addSource(p.Sources, SourceCompanyWebsite)
		}
	}
	p.Company = co
	return nil
}

// previous is the profile stored on the contact, or nil.
func previous(c *ent.Contact) *Profile {
	raw, ok := c.Metadata[MetadataKey]
	if !ok {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var p Profile
	if json.Unmarshal(b, &p) != nil {
		return nil
	}
	return &p
}

// save merges the profile into the contact's metadata in one UPDATE (metadata || patch), so no
// other metadata key is touched. It keeps updated_at as it was: the profile is derived data, and
// the follow-up agent reads updated_at as "last touched by a person". The update only applies
// while updated_at still matches what Build read, so a contact edited meanwhile is never
// overwritten with a profile of its older self, and never once it is erased.
func (e *Enricher) save(ctx context.Context, c *ent.Contact, p *Profile) (bool, error) {
	patch, err := json.Marshal(map[string]any{MetadataKey: p})
	if err != nil {
		return false, fmt.Errorf("encode profile: %w", err)
	}
	// ExprFunc with b.Arg so the builder numbers the placeholder; a "?" inside sql.Expr is not
	// rewritten in an UPDATE's SET clause.
	value := sql.ExprFunc(func(b *sql.Builder) {
		b.WriteString("(COALESCE(" + contact.FieldMetadata + ", '{}'::jsonb) || ")
		b.Arg(string(patch))
		b.WriteString("::jsonb)")
	})
	n, err := e.db.Contact.Update().
		Where(contact.ID(c.ID), contact.TenantID(c.TenantID), contact.UpdatedAtEQ(c.UpdatedAt), NotErased()).
		SetUpdatedAt(c.UpdatedAt).
		Modify(func(u *sql.UpdateBuilder) { u.Set(contact.FieldMetadata, value) }).
		Save(ctx)
	if err != nil {
		return false, fmt.Errorf("save profile: %w", err)
	}
	return n > 0, nil
}

// Stale selects the contacts of a tenant whose profile is missing or was refreshed before
// cutoff, never-enriched first, then the oldest. Erased contacts are left out.
func Stale(tenantID uuid.UUID, cutoff time.Time) (preds []predicate.Contact, order contact.OrderOption) {
	at := func(s *sql.Selector) string {
		return "(" + s.C(contact.FieldMetadata) + " -> '" + MetadataKey + "' ->> 'refreshed_at')"
	}
	preds = []predicate.Contact{
		contact.TenantID(tenantID),
		NotErased(),
		func(s *sql.Selector) {
			s.Where(sql.P(func(b *sql.Builder) {
				b.WriteString("(" + at(s) + " IS NULL OR " + at(s) + " < ")
				b.Arg(stamp(cutoff))
				b.WriteString(")")
			}))
		},
	}
	order = func(s *sql.Selector) {
		s.OrderExpr(sql.Expr(at(s) + " ASC NULLS FIRST"))
		s.OrderBy(s.C(contact.FieldID))
	}
	return preds, order
}
