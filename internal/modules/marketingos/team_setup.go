package marketingos

import (
	"context"
	"strings"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/ent/funnel"
	"github.com/bengobox/marketflow-api/internal/ent/knowledgebaseentry"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Team setup fits the prebuilt Marketing OS team (the defaults in marketflow-ai team.yaml) to one
// business from what MarketFlow and the other services already know. It is a prebuilt workflow
// with no model call: facts are indexed existence checks and read-only S2S reads, and rules turn
// them into safe changes (owner decision 2026-11-08):
//
//   - a member that cannot work for this business yet is switched off, with the reason and what
//     would switch it back on; when the fact changes, the next review switches it back on;
//   - a member the owner set by hand is never changed;
//   - nothing that spends, publishes or connects is touched.
//
// The first setup is free; marketflow-ai charges later reviews as a normal turn.

const (
	// SetByTeamSetup marks a member change the setup made, so a review may undo it.
	SetByTeamSetup = "team_setup"
	// TeamSetupKey holds the latest setup in Tenant.metadata.
	TeamSetupKey = "team_setup"
	// teamSetupVersion is bumped when the rules change, so reviews can tell an old setup.
	teamSetupVersion = 1
	// profileBudget bounds the auth-api read of the website; a slow answer leaves it unknown.
	profileBudget = 3 * time.Second
)

// WebPresence reads whether the business has a website or an online store (auth-api owns both).
// known is false when it could not be read, and the rules then leave website-bound members alone.
type WebPresence func(ctx context.Context, tenantID uuid.UUID) (has, known bool)

// WithWebPresence wires the auth-api reader.
func (s *Service) WithWebPresence(f WebPresence) *Service {
	s.webPresence = f
	return s
}

// Catalogue reports whether the business sells anything in its own systems (inventory, POS
// promotions, the codevertex-website catalogue for the platform tenant), read only through the
// creative offering sources. Used when the knowledge base lists no offerings.
type Catalogue func(ctx context.Context, tenantID uuid.UUID) bool

// WithCatalogue wires the catalogue check.
func (s *Service) WithCatalogue(f Catalogue) *Service {
	s.catalogue = f
	return s
}

// TeamFacts are what the rules look at. They describe capability, never personal data.
type TeamFacts struct {
	Website      bool `json:"website"`
	WebsiteKnown bool `json:"website_known"`
	WhatsApp     bool `json:"whatsapp"`
	SocialPages  bool `json:"social_pages"`
	AdAccount    bool `json:"ad_account"`
	PaidAdsPlan  bool `json:"paid_ads_plan"`
	LeadCapture  bool `json:"lead_capture"`
	Offerings    bool `json:"offerings"`
	BrandContext bool `json:"brand_context"`
	Competitors  bool `json:"competitors"`
}

// MemberFit is the setup's verdict on one member.
type MemberFit struct {
	Key     string `json:"key"`
	Enabled bool   `json:"enabled"`
	// Reason says why a member is off, in the owner's terms; empty when it is on.
	Reason string `json:"reason,omitempty"`
	// Limits are what an enabled member cannot do yet and what would allow it.
	Limits []string `json:"limits,omitempty"`
	// Changed is true when this setup switched the member on or off.
	Changed bool `json:"changed,omitempty"`
	// OwnerSet is true when the owner set this member by hand, so the setup left it.
	OwnerSet bool `json:"owner_set,omitempty"`
}

// TeamSetup is the outcome marketflow-ai turns into the org chart report.
type TeamSetup struct {
	First   bool        `json:"first"`
	At      string      `json:"at"`
	Version int         `json:"version"`
	Facts   TeamFacts   `json:"facts"`
	Members []MemberFit `json:"members"`
}

// teamKeys are the default members the rules know, in report order. The manager and the
// compliance reviewer are always on: one is the entry point, the other guards every draft.
var teamKeys = []string{"manager", "strategist", "research_analyst", "ads_specialist", "content_creator",
	"leads_manager", "sales_rep", "performance_analyst", "seo_specialist", "compliance_reviewer"}

// fitTeam applies the rules to the facts. It is pure, so the rules are tested on their own.
func fitTeam(f TeamFacts) []MemberFit {
	out := make([]MemberFit, 0, len(teamKeys))
	for _, k := range teamKeys {
		m := MemberFit{Key: k, Enabled: true}
		switch k {
		case "strategist", "content_creator":
			if !f.BrandContext {
				m.Limits = append(m.Limits, "works from general knowledge until your brand context is filled in")
			}
			if !f.Offerings {
				m.Limits = append(m.Limits, "has no products or services to promote until your catalogue or knowledge base lists them")
			}
		case "research_analyst":
			if !f.Competitors {
				m.Limits = append(m.Limits, "scans competitor ads once you list your competitors")
			}
		case "ads_specialist":
			if !f.AdAccount {
				m.Limits = append(m.Limits, "writes and tests ad copy; launching paid ads needs a connected ad account")
			} else if !f.PaidAdsPlan {
				m.Limits = append(m.Limits, "writes and tests ad copy; launching paid ads needs a plan with paid ads")
			}
		case "leads_manager":
			if !f.LeadCapture {
				m.Limits = append(m.Limits, "starts tagging and nurturing once a form or funnel brings in leads")
			}
		case "performance_analyst":
			if !f.LeadCapture && !f.SocialPages && !f.AdAccount {
				m.Limits = append(m.Limits, "reports on results once leads come in or a page or ad account is connected")
			}
		case "sales_rep":
			if !f.WhatsApp && !f.LeadCapture {
				m.Enabled, m.Reason = false, "has no leads to follow up yet: add a WhatsApp number or publish a lead form to switch it on"
			}
		case "seo_specialist":
			if f.WebsiteKnown && !f.Website {
				m.Enabled, m.Reason = false, "has no website or online store to improve yet: add one to switch it on"
			}
		}
		out = append(out, m)
	}
	return out
}

// SetupTeam gathers the facts, applies the safe changes and records the setup. The first call
// for a tenant reports First, which marketflow-ai uses to make it free.
func (s *Service) SetupTeam(ctx context.Context, tenantID uuid.UUID) (TeamSetup, error) {
	t, err := s.db.Tenant.Get(ctx, tenantID)
	if err != nil {
		return TeamSetup{}, err
	}
	_, done := t.Metadata[TeamSetupKey].(map[string]any)
	facts := s.teamFacts(ctx, t)
	fits := fitTeam(facts)

	overrides, err := s.TeamOverrides(ctx, tenantID)
	if err != nil {
		return TeamSetup{}, err
	}
	byKey := make(map[string]MemberOverride, len(overrides))
	for _, o := range overrides {
		byKey[o.Key] = o
	}
	for i := range fits {
		m := &fits[i]
		o, has := byKey[m.Key]
		if has && o.SetBy != SetByTeamSetup {
			// The owner's own choice stands; report what it is.
			m.OwnerSet, m.Enabled = true, !o.Disabled
			if !m.Enabled {
				m.Reason = "switched off by you"
			}
			continue
		}
		current := !has || !o.Disabled
		if current == m.Enabled {
			continue
		}
		// Keep any name and instructions on the row; only the switch changes.
		o.Key, o.Disabled, o.SetBy = m.Key, !m.Enabled, SetByTeamSetup
		if _, err := s.UpsertTeamMember(ctx, tenantID, o); err != nil {
			return TeamSetup{}, err
		}
		m.Changed = true
	}

	now := time.Now().UTC().Format(time.RFC3339)
	out := TeamSetup{First: !done, At: now, Version: teamSetupVersion, Facts: facts, Members: fits}
	record := map[string]any{"at": now, "version": teamSetupVersion}
	if prev, ok := t.Metadata[TeamSetupKey].(map[string]any); ok && str(prev["first_at"]) != "" {
		record["first_at"] = prev["first_at"]
	} else {
		record["first_at"] = now
	}
	if err := s.setKeys(ctx, tenantID, map[string]any{TeamSetupKey: record}); err != nil {
		return TeamSetup{}, err
	}
	return out, nil
}

// SetupTeamIfNew runs the first setup once, for onboarding: a tenant that already has one is
// left alone. Errors are logged, never returned, because it runs after the owner's own save.
func (s *Service) SetupTeamIfNew(ctx context.Context, tenantID uuid.UUID) {
	t, err := s.db.Tenant.Get(ctx, tenantID)
	if err != nil {
		return
	}
	if _, done := t.Metadata[TeamSetupKey].(map[string]any); done {
		return
	}
	if _, err := s.SetupTeam(ctx, tenantID); err != nil {
		s.logger.Warn("first team setup", zap.String("tenant_id", tenantID.String()), zap.Error(err))
	}
}

// teamFacts runs one indexed existence check per fact; a check that fails counts as absent,
// which only ever adds a limit or keeps a member off, never spends anything.
func (s *Service) teamFacts(ctx context.Context, t *ent.Tenant) TeamFacts {
	f := TeamFacts{WhatsApp: t.WhatsappNumber != nil && strings.TrimSpace(*t.WhatsappNumber) != ""}
	types, _ := s.db.ConnectedAccount.Query().
		Where(connectedaccount.TenantID(t.ID), connectedaccount.Status("active")).
		Unique(true).Select(connectedaccount.FieldAccountType).Strings(ctx)
	for _, ty := range types {
		switch ty {
		case "meta_ad_account", "tiktok_advertiser", "google_ads_customer":
			f.AdAccount = true
		default:
			f.SocialPages = true
		}
	}
	f.LeadCapture, _ = s.db.Funnel.Query().Where(funnel.TenantID(t.ID), funnel.Status("published")).Exist(ctx)
	if !f.LeadCapture {
		f.LeadCapture, _ = s.db.Lead.Query().Where(lead.TenantID(t.ID)).Exist(ctx)
	}
	f.Offerings, _ = s.db.KnowledgeBaseEntry.Query().
		Where(knowledgebaseentry.TenantID(t.ID), knowledgebaseentry.CategoryIn(offeringCategories...)).Exist(ctx)
	if !f.Offerings {
		f.Offerings = len(brandOffers(t.Metadata)) > 0
	}
	if !f.Offerings && s.catalogue != nil {
		cctx, cancel := context.WithTimeout(ctx, profileBudget)
		f.Offerings = s.catalogue(cctx, t.ID)
		cancel()
	}
	if bc, ok := t.Metadata[MetadataKey].(map[string]any); ok {
		f.BrandContext = strings.TrimSpace(str(bc["positioning"])) != ""
	}
	f.Competitors = len(readStrings(t.Metadata, CompetitorsKey)) > 0
	if s.subs != nil {
		if e := s.subs.GetEntitlements(ctx, t.ID.String()); e != nil {
			for _, feat := range e.Features {
				if feat == "paid_ads_launch" {
					f.PaidAdsPlan = true
				}
			}
			f.PaidAdsPlan = f.PaidAdsPlan || e.IsDemoBypass
		}
	}
	if s.webPresence != nil {
		wctx, cancel := context.WithTimeout(ctx, profileBudget)
		f.Website, f.WebsiteKnown = s.webPresence(wctx, t.ID)
		cancel()
	}
	return f
}
