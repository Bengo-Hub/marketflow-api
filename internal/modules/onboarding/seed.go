package onboarding

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	enttenant "github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/bengobox/marketflow-api/internal/modules/marketingos"
	"github.com/bengobox/marketflow-api/internal/modules/tenant"
	"github.com/bengobox/marketflow-api/internal/modules/tenant/metastore"
)

// Seed defaults (owner, 2026-11-19: "all active tenants, fill gaps only"). A platform owner run
// that gives a tenant sensible starting values from what its business already has elsewhere, so
// the owner edits rather than starts from a blank page:
//
//   - the brand context, from the same draft the onboarding wizard shows (profile, account record,
//     best sellers or the website catalogue, testimonials) and the website's own description;
//   - the profile's contact links, from the account record in auth-api;
//   - a few FAQ entries from the website and the account record, kept as pending suggestions the
//     owner approves, each marked with its source and date, only where the knowledge base says
//     nothing on that topic;
//   - the defaults the platform already ships and an older tenant may lack: the Forever profile
//     defaults for a Forever tenant and the first team setup.
//
// It only ever fills an empty field. Every value comes from a real source; a field no source
// covers (the audience, the voice) is skipped and reported, never guessed. Running it again
// changes nothing, because everything it filled is no longer empty.

// SeedDeps are the writers and readers the run needs beyond the draft. The app wires them from
// their owners: marketingos (the only brand context writer, business facts, team setup) and the
// knowledge base. A nil function turns that part off.
type SeedDeps struct {
	SaveBrand     func(ctx context.Context, tenantID uuid.UUID, bc marketingos.BrandContext) (marketingos.BrandContext, error)
	Facts         func(ctx context.Context, tenantID uuid.UUID) (marketingos.BusinessFacts, error)
	KBTitles      func(ctx context.Context, tenantID uuid.UUID) ([]string, error)
	DraftFAQ      func(ctx context.Context, tenantID uuid.UUID, question, answer, source, sourceURL string) (bool, error)
	TeamSetupDone func(ctx context.Context, tenantID uuid.UUID) (bool, error)
	SetupTeam     func(ctx context.Context, tenantID uuid.UUID)
}

// WithSeed wires the seed defaults run.
func (s *Service) WithSeed(d SeedDeps) *Service {
	s.seed = d
	return s
}

// SeedItem is one field the run filled (or would fill), or skipped.
type SeedItem struct {
	Area      string `json:"area"` // brand_context | profile | knowledge | defaults
	Field     string `json:"field"`
	Source    string `json:"source,omitempty"`
	SourceURL string `json:"source_url,omitempty"`
	Value     string `json:"value,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// SeedReport is what the run did for one tenant.
type SeedReport struct {
	TenantID     string     `json:"tenant_id"`
	Slug         string     `json:"slug"`
	Name         string     `json:"name"`
	BusinessType string     `json:"business_type"`
	DryRun       bool       `json:"dry_run"`
	Filled       []SeedItem `json:"filled"`
	Skipped      []SeedItem `json:"skipped"`
	Errors       []string   `json:"errors,omitempty"`
}

// seedBudget bounds one tenant: the draft (4s), the business facts (auth, website, crawler) and
// the writes.
const seedBudget = 45 * time.Second

// reportValueLen keeps the report readable; the stored value is never cut by it.
const reportValueLen = 140

// faqTopic is a question the run can draft when the knowledge base has nothing on it. words are
// matched against the lowercase titles of every entry the tenant owns.
type faqTopic struct {
	key      string
	question string
	words    []string
}

var faqTopics = []faqTopic{
	{"about", "What does %s do?", []string{"about", "what do you do", "what does", "who are you", "who we are", "what is "}},
	{"location", "Where is %s located?", []string{"locat", "address", "where are you", "where is", "directions", "find you"}},
	{"hours", "What are the opening hours at %s?", []string{"hour", "open", "closing"}},
	{"contact", "How do I contact %s?", []string{"contact", "reach", "phone", "call", "email", "whatsapp"}},
}

// faqDraft is one FAQ entry the run would add.
type faqDraft struct {
	topic, question, answer, source, url string
}

// seedInput is everything the plan looks at; planSeed does no I/O.
type seedInput struct {
	tenant  *ent.Tenant
	current marketingos.BrandContext
	draft   Draft
	facts   *marketingos.BusinessFacts
	titles  []string
	// titlesKnown is false when the knowledge base could not be read; no FAQ is drafted then.
	titlesKnown bool
	teamDone    bool
	teamKnown   bool
}

// seedPlan is what the run would write.
type seedPlan struct {
	brand        marketingos.BrandContext // only the fields to fill are set
	brandFields  []string
	profile      map[string]any // tenant metadata keys to fill
	faqs         []faqDraft
	runTeamSetup bool
	report       SeedReport
}

func (p *seedPlan) fill(area, field, source, url, value string) {
	p.report.Filled = append(p.report.Filled, SeedItem{Area: area, Field: field, Source: source, SourceURL: url, Value: clip(value, reportValueLen)})
}

func (p *seedPlan) skip(area, field, reason string) {
	p.report.Skipped = append(p.report.Skipped, SeedItem{Area: area, Field: field, Reason: reason})
}

// planSeed decides every fill. A field that already holds a value is never in the plan.
func planSeed(in seedInput) seedPlan {
	t := in.tenant
	meta := t.Metadata
	p := seedPlan{profile: map[string]any{}, report: SeedReport{TenantID: t.ID.String(), Slug: t.Slug, Name: t.Name, BusinessType: t.BusinessType}}
	cur, sug := in.current, in.draft.Suggested
	website := ""
	if in.facts != nil {
		website = in.facts.Website
	}

	// Brand context.
	if strings.TrimSpace(cur.Positioning) != "" {
		p.skip("brand_context", "positioning", "already set")
	} else if v := str(meta, "bio"); v != "" {
		p.brand.Positioning = clip(v, 1200)
		p.fill("brand_context", "positioning", "profile bio", "", p.brand.Positioning)
	} else if v := str(meta, "tagline"); v != "" {
		p.brand.Positioning = clip(v, 1200)
		p.fill("brand_context", "positioning", "profile tagline", "", p.brand.Positioning)
	} else if in.facts != nil && strings.TrimSpace(in.facts.Summary) != "" {
		p.brand.Positioning = clip(in.facts.Summary, 1200)
		p.fill("brand_context", "positioning", "website description", website, p.brand.Positioning)
	} else {
		p.skip("brand_context", "positioning", "no bio, tagline or website description to take it from")
	}

	if len(cur.Offers) > 0 {
		p.skip("brand_context", "offers", "already set")
	} else if len(sug.Offers) > 0 {
		p.brand.Offers = cap20(sug.Offers)
		p.fill("brand_context", "offers", offeringSources(in.draft.Offerings), "", strings.Join(p.brand.Offers, "; "))
	} else if names := featuredNames(meta); strings.EqualFold(t.BusinessType, tenant.BusinessTypeFLP) && len(names) > 0 {
		p.brand.Offers = names
		p.fill("brand_context", "offers", "featured Forever products", "", strings.Join(names, "; "))
	} else {
		p.skip("brand_context", "offers", "no sales, inventory or catalogue items found")
	}

	if len(cur.Proof) > 0 {
		p.skip("brand_context", "proof", "already set")
	} else if len(sug.Proof) > 0 {
		p.brand.Proof = cap20(sug.Proof)
		src := "published testimonials"
		if in.draft.hasSource("website") {
			src = "published testimonials and website catalogue highlights"
		}
		p.fill("brand_context", "proof", src, "", strings.Join(p.brand.Proof, "; "))
	} else {
		p.skip("brand_context", "proof", "no published testimonials")
	}

	if len(cur.BannedClaims) > 0 {
		p.skip("brand_context", "banned_claims", "already set")
	} else if len(sug.BannedClaims) > 0 {
		p.brand.BannedClaims = cap20(sug.BannedClaims)
		p.fill("brand_context", "banned_claims", "platform compliance rules for business type "+firstNonEmpty(t.BusinessType, tenant.BusinessTypeGeneric), "", strings.Join(p.brand.BannedClaims, "; "))
	}

	// No source says who buys or how the business speaks; the owner sets these.
	if len(cur.Audience) == 0 {
		p.skip("brand_context", "audience", "no source names the customers; left for the owner")
	}
	if strings.TrimSpace(cur.Voice) == "" {
		p.skip("brand_context", "voice", "no source states the tone; left for the owner")
	}
	for _, f := range []struct {
		name string
		set  bool
	}{{"positioning", p.brand.Positioning != ""}, {"offers", len(p.brand.Offers) > 0}, {"proof", len(p.brand.Proof) > 0}, {"banned_claims", len(p.brand.BannedClaims) > 0}} {
		if f.set {
			p.brandFields = append(p.brandFields, f.name)
		}
	}

	// Profile contact links from the account record (the draft only offers empty ones).
	prof := in.draft.Profile
	isFLP := strings.EqualFold(t.BusinessType, tenant.BusinessTypeFLP)
	for _, f := range []struct{ key, value string }{
		{"contact_email", prof.ContactEmail}, {"logo_url", prof.LogoURL}, {"brand_primary_color", prof.BrandColor},
	} {
		switch {
		case str(meta, f.key) != "":
			p.skip("profile", f.key, "already set")
		case isFLP && f.key == "logo_url":
			// A Forever tenant always uses the Forever Living logo (owner, 2026-11-19): the
			// platform default below, never the account record's.
			p.skip("profile", f.key, "a Forever tenant uses the Forever Living logo from the platform defaults")
		case strings.TrimSpace(f.value) != "":
			p.profile[f.key] = strings.TrimSpace(f.value)
			p.fill("profile", f.key, "account record (auth-api)", "", f.value)
		default:
			p.skip("profile", f.key, "the account record has none")
		}
	}
	socials, _ := meta["socials"].(map[string]any)
	if w, _ := socials["website"].(string); strings.TrimSpace(w) != "" {
		p.skip("profile", "socials.website", "already set")
	} else if site := strings.TrimSpace(prof.Website); strings.HasPrefix(site, "https://") || strings.HasPrefix(site, "http://") {
		p.profile["socials.website"] = site
		p.fill("profile", "socials.website", "account record website (auth-api)", site, site)
	} else {
		p.skip("profile", "socials.website", "the account record has no website")
	}
	if (t.WhatsappNumber == nil || strings.TrimSpace(*t.WhatsappNumber) == "") && prof.WhatsApp != "" {
		p.skip("profile", "whatsapp_number", "the account phone is not confirmed as a WhatsApp number; left for the owner")
	}

	// Platform defaults a tenant synced before they existed may lack.
	if isFLP {
		// The old default lived on both Codevertex account domains over time.
		if strings.HasSuffix(str(meta, "logo_url"), "/images/logo/codevertex.png") {
			p.skip("defaults", "logo_url", "holds the Codevertex logo from the old Forever default; left as stored for the owner to review")
		}
		missing := tenant.MissingFBODefaults(meta)
		keys := make([]string, 0, len(missing))
		for k := range missing {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p.profile[k] = missing[k]
			p.fill("defaults", k, "platform Forever profile defaults", "", fmt.Sprint(missing[k]))
		}
	}
	switch {
	case !in.teamKnown:
	case in.teamDone:
		p.skip("defaults", "team_setup", "already done")
	default:
		p.runTeamSetup = true
		p.fill("defaults", "team_setup", "platform team rules (members that cannot work yet are switched off; owner choices kept)", "", "first team setup")
	}

	// FAQ drafts, one per topic the knowledge base says nothing about.
	if in.facts != nil && in.titlesKnown {
		p.planFAQs(in)
	} else {
		p.skip("knowledge", "faq", "the business facts or the knowledge base could not be read")
	}
	return p
}

func (p *seedPlan) planFAQs(in seedInput) {
	f := in.facts
	name := firstNonEmpty(f.Name, in.tenant.Name)
	lower := make([]string, len(in.titles))
	for i, t := range in.titles {
		lower[i] = strings.ToLower(t)
	}
	covered := func(words []string) bool {
		for _, t := range lower {
			for _, w := range words {
				if strings.Contains(t, w) {
					return true
				}
			}
		}
		return false
	}
	for _, topic := range faqTopics {
		field := "faq." + topic.key
		if covered(topic.words) {
			p.skip("knowledge", field, "the knowledge base already has an entry on this topic")
			continue
		}
		var d faqDraft
		switch topic.key {
		case "about":
			if s := strings.TrimSpace(f.Summary); s != "" {
				d = faqDraft{answer: strings.TrimRight(s, ".") + ".", source: "website description", url: f.Website}
			}
		case "location":
			if a := strings.TrimSpace(f.Address); a != "" {
				d = faqDraft{answer: fmt.Sprintf("%s is at %s.", name, strings.TrimRight(a, ".")), source: "website business details", url: f.Website}
			}
		case "hours":
			if len(f.Hours) > 0 {
				d = faqDraft{answer: fmt.Sprintf("%s's opening hours: %s.", name, strings.Join(f.Hours, "; ")), source: "website business details", url: f.Website}
			}
		case "contact":
			var lines []string
			if f.Phone != "" {
				lines = append(lines, "Phone: "+f.Phone)
			}
			if f.WhatsApp != "" {
				lines = append(lines, "WhatsApp: "+f.WhatsApp)
			}
			if f.Email != "" {
				lines = append(lines, "Email: "+f.Email)
			}
			if f.Website != "" {
				lines = append(lines, "Website: "+f.Website)
			}
			if len(lines) > 0 {
				d = faqDraft{answer: fmt.Sprintf("You can reach %s here.\n%s", name, strings.Join(lines, "\n")), source: "account record and profile", url: f.Website}
			}
		}
		if d.answer == "" {
			p.skip("knowledge", field, "no source states it")
			continue
		}
		d.topic, d.question = topic.key, fmt.Sprintf(topic.question, name)
		p.faqs = append(p.faqs, d)
		p.fill("knowledge", field, d.source+" (pending suggestion for the owner)", d.url, d.question+" "+d.answer)
	}
}

// offeringSources names where the draft's offerings came from.
func offeringSources(list []Suggestion) string {
	seen := map[string]bool{}
	var out []string
	names := map[string]string{"pos": "POS best sellers", "inventory": "inventory items", "website": "website catalogue", "course": "website courses"}
	for _, o := range list {
		if n := names[o.Source]; n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return "catalogue"
	}
	return strings.Join(out, ", ")
}

// featuredNames are the names of a tenant's featured products (metadata.offerings).
func featuredNames(meta map[string]any) []string {
	list, _ := meta["offerings"].([]any)
	var out []string
	for _, raw := range list {
		if m, ok := raw.(map[string]any); ok {
			if n, _ := m["name"].(string); strings.TrimSpace(n) != "" && len(out) < 20 {
				out = append(out, clip(n, itemLen))
			}
		}
	}
	return out
}

func (d Draft) hasSource(name string) bool {
	for _, s := range d.Sources {
		if s == name {
			return true
		}
	}
	return false
}

// exclude drops fills the platform owner turned down. Each entry is "slug:area/field" (for one
// tenant) or "area/field" (for every tenant). An excluded fill is reported as skipped.
func (p *seedPlan) exclude(slug string, list []string) {
	if len(list) == 0 {
		return
	}
	drop := map[string]bool{}
	for _, e := range list {
		e = strings.TrimSpace(e)
		if s, rest, ok := strings.Cut(e, ":"); ok {
			if s == slug {
				drop[rest] = true
			}
			continue
		}
		drop[e] = true
	}
	kept := p.report.Filled[:0]
	for _, it := range p.report.Filled {
		key := it.Area + "/" + it.Field
		if !drop[key] {
			kept = append(kept, it)
			continue
		}
		p.skip(it.Area, it.Field, "excluded by the platform owner")
		switch {
		case key == "brand_context/positioning":
			p.brand.Positioning = ""
		case key == "brand_context/offers":
			p.brand.Offers = nil
		case key == "brand_context/proof":
			p.brand.Proof = nil
		case key == "brand_context/banned_claims":
			p.brand.BannedClaims = nil
		case key == "defaults/team_setup":
			p.runTeamSetup = false
		case it.Area == "profile" || it.Area == "defaults":
			delete(p.profile, it.Field)
		case it.Area == "knowledge":
			faqs := p.faqs[:0]
			for _, f := range p.faqs {
				if it.Field != "faq."+f.topic {
					faqs = append(faqs, f)
				}
			}
			p.faqs = faqs
		}
	}
	p.report.Filled = kept
	fields := p.brandFields[:0]
	for _, f := range p.brandFields {
		if !drop["brand_context/"+f] {
			fields = append(fields, f)
		}
	}
	p.brandFields = fields
}

// SeedDefaults runs the seed for one tenant. dryRun reads every source and reports, writing
// nothing. exclude lists fills to leave out (seedPlan.exclude).
func (s *Service) SeedDefaults(ctx context.Context, tenantID uuid.UUID, dryRun bool, exclude ...string) (SeedReport, error) {
	ctx, cancel := context.WithTimeout(ctx, seedBudget)
	defer cancel()
	t, err := s.db.Tenant.Get(ctx, tenantID)
	if err != nil {
		return SeedReport{}, fmt.Errorf("load tenant: %w", err)
	}
	draft, err := s.Draft(ctx, tenantID)
	if err != nil {
		return SeedReport{}, err
	}
	in := seedInput{tenant: t, current: draft.Current, draft: draft}
	var errs []string
	if s.seed.Facts != nil {
		if f, err := s.seed.Facts(ctx, tenantID); err == nil {
			in.facts = &f
		} else {
			errs = append(errs, "business facts: "+err.Error())
		}
	}
	if s.seed.KBTitles != nil && s.seed.DraftFAQ != nil {
		if titles, err := s.seed.KBTitles(ctx, tenantID); err == nil {
			in.titles, in.titlesKnown = titles, true
		} else {
			errs = append(errs, "knowledge base: "+err.Error())
		}
	}
	if s.seed.TeamSetupDone != nil && s.seed.SetupTeam != nil {
		if done, err := s.seed.TeamSetupDone(ctx, tenantID); err == nil {
			in.teamDone, in.teamKnown = done, true
		}
	}
	plan := planSeed(in)
	plan.exclude(t.Slug, exclude)
	rep := plan.report
	rep.DryRun = dryRun
	rep.Errors = append(rep.Errors, errs...)
	if dryRun {
		return rep, nil
	}
	rep.Errors = append(rep.Errors, s.applySeed(ctx, tenantID, plan)...)
	return rep, nil
}

// applySeed writes the plan. Each write reads the stored value again first and fills only what is
// still empty, so an edit the owner made since the plan was read always wins.
func (s *Service) applySeed(ctx context.Context, tenantID uuid.UUID, plan seedPlan) []string {
	var errs []string
	if len(plan.brandFields) > 0 && s.seed.SaveBrand != nil {
		if err := s.fillBrand(ctx, tenantID, plan.brand); err != nil {
			errs = append(errs, "brand context: "+err.Error())
		}
	}
	if len(plan.profile) > 0 {
		if err := s.fillProfile(ctx, tenantID, plan.profile); err != nil {
			errs = append(errs, "profile: "+err.Error())
		}
	}
	for _, f := range plan.faqs {
		if _, err := s.seed.DraftFAQ(ctx, tenantID, f.question, f.answer, f.source, f.url); err != nil {
			errs = append(errs, "faq "+f.question+": "+err.Error())
		}
	}
	if plan.runTeamSetup {
		s.seed.SetupTeam(ctx, tenantID)
	}
	return errs
}

func (s *Service) fillBrand(ctx context.Context, tenantID uuid.UUID, add marketingos.BrandContext) error {
	bc, err := s.brand.GetBrandContext(ctx, tenantID)
	if err != nil {
		return err
	}
	changed := false
	if strings.TrimSpace(bc.Positioning) == "" && add.Positioning != "" {
		bc.Positioning, changed = add.Positioning, true
	}
	for _, f := range []struct{ dst, src *[]string }{
		{&bc.Offers, &add.Offers}, {&bc.Proof, &add.Proof}, {&bc.BannedClaims, &add.BannedClaims},
	} {
		if len(*f.dst) == 0 && len(*f.src) > 0 {
			*f.dst, changed = *f.src, true
		}
	}
	if !changed {
		return nil
	}
	_, err = s.seed.SaveBrand(ctx, tenantID, bc)
	return err
}

func (s *Service) fillProfile(ctx context.Context, tenantID uuid.UUID, add map[string]any) error {
	t, err := s.db.Tenant.Query().Where(enttenant.ID(tenantID)).Only(ctx)
	if err != nil {
		return err
	}
	patch := map[string]any{}
	for k, v := range add {
		if k == "socials.website" {
			socials := map[string]any{}
			if cur, ok := t.Metadata["socials"].(map[string]any); ok {
				for sk, sv := range cur {
					socials[sk] = sv
				}
			}
			if w, _ := socials["website"].(string); strings.TrimSpace(w) == "" {
				socials["website"] = v
				patch["socials"] = socials
			}
			continue
		}
		if _, present := t.Metadata[k]; present {
			if sv, isStr := t.Metadata[k].(string); !isStr || strings.TrimSpace(sv) != "" {
				continue
			}
		}
		patch[k] = v
	}
	return metastore.SetKeys(ctx, s.db, tenantID, patch)
}

// Bounds of the all-tenants run: a page of tenants per request, a few at a time, so one request
// stays well inside the proxy's timeout.
const (
	seedPageDefault = 5
	seedPageMax     = 20
	seedParallel    = 4
)

// SeedPage is one page of the all-tenants run.
type SeedPage struct {
	Total   int          `json:"total"`
	Offset  int          `json:"offset"`
	Limit   int          `json:"limit"`
	DryRun  bool         `json:"dry_run"`
	Tenants []SeedReport `json:"tenants"`
}

// SeedActiveTenants runs the seed for one page of active tenants, ordered by slug.
func (s *Service) SeedActiveTenants(ctx context.Context, offset, limit int, dryRun bool, exclude ...string) (SeedPage, error) {
	if limit <= 0 {
		limit = seedPageDefault
	}
	if limit > seedPageMax {
		limit = seedPageMax
	}
	if offset < 0 {
		offset = 0
	}
	q := s.db.Tenant.Query().Where(enttenant.StatusEQ("active"))
	total, err := q.Clone().Count(ctx)
	if err != nil {
		return SeedPage{}, err
	}
	ids, err := q.Order(ent.Asc(enttenant.FieldSlug)).Offset(offset).Limit(limit).IDs(ctx)
	if err != nil {
		return SeedPage{}, err
	}
	out := SeedPage{Total: total, Offset: offset, Limit: limit, DryRun: dryRun, Tenants: make([]SeedReport, len(ids))}
	sem := make(chan struct{}, seedParallel)
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, id uuid.UUID) {
			defer wg.Done()
			defer func() { <-sem }()
			rep, err := s.SeedDefaults(ctx, id, dryRun, exclude...)
			if err != nil {
				rep = SeedReport{TenantID: id.String(), DryRun: dryRun, Errors: []string{err.Error()}}
			}
			out.Tenants[i] = rep
		}(i, id)
	}
	wg.Wait()
	return out, nil
}
