// Package marketingos owns the Marketing OS data that lives in marketflow-api:
// the tenant's brand context (Tenant.metadata.marketing_context), the team member
// overrides (CustomAgent rows with metadata.kind = "team_member"), the context
// marketflow-ai reads for every request, and the daily brief metrics. The brain
// itself (routing, playbooks, models) is in marketflow-ai.
package marketingos

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqljson"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/platform/shareddefaults"
	"github.com/bengobox/marketflow-api/internal/ent/customagent"
	"github.com/bengobox/marketflow-api/internal/ent/knowledgebaseentry"
	"github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/bengobox/marketflow-api/internal/modules/tenant/metastore"
	"github.com/bengobox/marketflow-api/internal/platform/subscriptions"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// MetadataKey is where the brand context lives in Tenant.metadata.
const MetadataKey = "marketing_context"

// KindTeamMember marks a CustomAgent row as a team member override.
const KindTeamMember = "team_member"

// DailyBriefKey in Tenant.metadata switches the morning brief off when false.
// Absent means on.
const DailyBriefKey = "os_daily_brief"

// BusinessTypeChosenKey in Tenant.metadata records when the business type was saved through the
// profile (RFC 3339). Only the profile PATCH writes it (tenant.ReservedMetadataKeys).
const BusinessTypeChosenKey = "business_type_chosen_at"

// BusinessTypeChosen reports whether the owner or a platform admin ever saved a business type.
func BusinessTypeChosen(meta map[string]any) bool {
	v, _ := meta[BusinessTypeChosenKey].(string)
	return v != ""
}

// Settings are the tenant's Marketing OS switches.
type Settings struct {
	DailyBrief bool `json:"daily_brief"`
	// InboxAutoReply answers buyer comments and DMs with InboxReplyText (Sprint 22). Off by
	// default; saving it on with a message is the owner's approval of that message.
	InboxAutoReply bool   `json:"inbox_auto_reply"`
	InboxReplyText string `json:"inbox_reply_text"`
	// InboxIntentThreshold and InboxAutoReplyThreshold override the default confidence bars
	// (inbox_settings.go); null means the default. InboxCommentPrompt is the public line under a
	// comment whose answer went as a private reply.
	InboxIntentThreshold    *float64 `json:"inbox_intent_threshold"`
	InboxAutoReplyThreshold *float64 `json:"inbox_auto_reply_threshold"`
	InboxCommentPrompt      string   `json:"inbox_comment_prompt"`
	// Competitors are names, domains or Google advertiser ids (AR...) the weekly competitor
	// scan looks up (Sprint 24), at most MaxCompetitors.
	Competitors []string `json:"competitors"`
	// ReferenceAds and CustomerComplaints feed the Find, Flip, Fix engine (market_signals.go).
	ReferenceAds       []ReferenceAd `json:"reference_ads"`
	CustomerComplaints []string      `json:"customer_complaints"`
	// ContactEnrichment lets the contact enricher fill each contact's profile from the
	// business's own records (on by default). ContactEnrichmentWeb also reads the public website
	// of the company a contact is linked to (off by default, and off whenever ContactEnrichment is).
	ContactEnrichment    bool `json:"contact_enrichment"`
	ContactEnrichmentWeb bool `json:"contact_enrichment_web"`
	// BoostOTPThresholdKES is the boost total (KES) from which a launch needs a WhatsApp
	// confirmation code; null means the default (boost_otp_setting.go), 0 turns it off.
	BoostOTPThresholdKES *int64 `json:"boost_otp_threshold_kes"`
}

// Tenant metadata keys of the contact enricher switches. An absent ContactEnrichmentKey means on;
// an absent ContactEnrichmentWebKey means off.
const (
	ContactEnrichmentKey    = "contact_enrichment"
	ContactEnrichmentWebKey = "contact_enrichment_web"
	// ContactEnrichmentClearKey marks stored profile data the worker must remove after a switch
	// was turned off: ClearAllProfiles or ClearCompanySummaries. The worker deletes the key when done.
	ContactEnrichmentClearKey = "contact_enrichment_clear"
	ClearAllProfiles          = "all"
	ClearCompanySummaries     = "company_summaries"
)

// ContactEnrichmentSwitches reads the two contact enricher switches from tenant metadata. The
// website lookup is never on while the enricher itself is off.
func ContactEnrichmentSwitches(meta map[string]any) (on, web bool) {
	v, set := meta[ContactEnrichmentKey].(bool)
	on = !set || v
	w, _ := meta[ContactEnrichmentWebKey].(bool)
	return on, on && w
}

// Tenant metadata keys of the inbox auto reply.
const (
	InboxAutoReplyKey = "inbox_auto_reply"
	InboxReplyTextKey = "inbox_reply_text"
	// MaxInboxReplyText keeps the reply short enough for every platform.
	MaxInboxReplyText = 500
	CompetitorsKey    = "competitors"
	MaxCompetitors    = 10
)

// BrandContext is the owner-approved description every specialist reads first.
type BrandContext struct {
	Positioning   string   `json:"positioning"`
	Audience      []string `json:"audience"`
	Offers        []string `json:"offers"`
	Proof         []string `json:"proof"`
	Voice         string   `json:"voice"`
	VoiceExamples []string `json:"voice_examples"`
	BannedClaims  []string `json:"banned_claims"`
	Learnings     []string `json:"learnings"`
	// SharedBannedClaims are the platform's claims this business inherits, read only. BannedClaims
	// are the ones it added itself; both apply (EffectiveContext).
	SharedBannedClaims []string `json:"shared_banned_claims,omitempty"`
	// ActorNameStyle is the naming tradition the names of on-screen actors follow (one of
	// ActorNameStyles); empty leaves it to the writer. ActorNames are exact names the business
	// likes; when there are any, the script writer uses only these (actor_names.go).
	ActorNameStyle string   `json:"actor_name_style,omitempty"`
	ActorNames     []string `json:"actor_names,omitempty"`
}

// Limits keep the context small enough to sit in every prompt.
const (
	maxTextLen  = 1200
	maxItemLen  = 300
	maxListLen  = 20
	maxLearning = 50
)

// Validate trims and bounds the context.
func (b *BrandContext) Validate() error {
	b.Positioning = strings.TrimSpace(b.Positioning)
	b.Voice = strings.TrimSpace(b.Voice)
	if len(b.Positioning) > maxTextLen || len(b.Voice) > maxTextLen {
		return fmt.Errorf("positioning and voice are limited to %d characters", maxTextLen)
	}
	lists := []*[]string{&b.Audience, &b.Offers, &b.Proof, &b.VoiceExamples, &b.BannedClaims}
	for _, l := range lists {
		clean, err := cleanList(*l, maxListLen)
		if err != nil {
			return err
		}
		*l = clean
	}
	clean, err := cleanList(b.Learnings, maxLearning)
	if err != nil {
		return err
	}
	b.Learnings = clean
	return b.validateActorNaming()
}

func cleanList(in []string, max int) ([]string, error) {
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if len(s) > maxItemLen {
			return nil, fmt.Errorf("list items are limited to %d characters", maxItemLen)
		}
		out = append(out, s)
	}
	if len(out) > max {
		return nil, fmt.Errorf("lists are limited to %d items", max)
	}
	return out, nil
}

// MemberOverride is a tenant's change to a default team member. The defaults
// themselves (roles, playbooks, tools) are code in marketflow-ai.
type MemberOverride struct {
	Key          string `json:"key"`
	Name         string `json:"name,omitempty"`
	Disabled     bool   `json:"disabled,omitempty"`
	ModelClass   string `json:"model_class,omitempty"`
	Instructions string `json:"instructions,omitempty"`
	// SetBy is "team_setup" when the setup made this change; empty when the owner did. The
	// setup never changes a member the owner set (team_setup.go).
	SetBy string `json:"set_by,omitempty"`
}

var (
	memberKeyRe  = regexp.MustCompile(`^[a-z][a-z_]{2,40}$`)
	modelClasses = map[string]bool{"": true, "nano": true, "standard": true, "premium": true, "premium_plus": true}
)

// OSContext is what marketflow-ai reads for each request.
type OSContext struct {
	BusinessType string `json:"business_type"`
	// BusinessTypeChosen is true once the owner (or a platform admin) saved a type through the
	// profile (BusinessTypeChosenKey). Until then marketflow-ai lets a widget persona or the chat
	// auto-detect replace a "generic" type; a chosen type, generic included, is final (owner,
	// 2026-11-14).
	BusinessTypeChosen bool `json:"business_type_chosen"`

	MarketingContext string           `json:"marketing_context"`
	Offerings        []string         `json:"offerings"`
	Members          []MemberOverride `json:"members"`
	Features         map[string]bool  `json:"features,omitempty"`
	// ChatTurnsDaily is the plan's free chat turns a day (subscriptions limit chat_turns_daily);
	// 0 when the plan sets none, and marketflow-ai keeps its default.
	ChatTurnsDaily int  `json:"chat_turns_daily,omitempty"`
	HasPlan        bool `json:"has_plan"`
	// Exempt is the platform owner's own tenant only (owner, 2026-11-12): marketflow-ai gives it
	// every plan level, such as the top video sound level, whatever its feature list says. Demo
	// tenants are NOT exempt here, unlike other services' gating: their password is public and
	// AI work costs real money, so they stay on their plan's features.
	Exempt bool `json:"exempt,omitempty"`
	// MarketSignals is the Find, Flip, Fix block (market_signals.go); absent until there is one.
	MarketSignals *MarketSignals `json:"market_signals,omitempty"`
	// PaymentMethods and Glossary are the team's terms (glossary.go): the only payment methods
	// it may name, and spellings it must correct.
	PaymentMethods []string       `json:"payment_methods,omitempty"`
	Glossary       []GlossaryTerm `json:"glossary,omitempty"`
}

// Service serves the Marketing OS data.
type Service struct {
	db          *ent.Client
	subs        *subscriptions.Client
	logger      *zap.Logger
	webPresence WebPresence // team_setup.go; nil leaves website-bound members alone
	catalogue   Catalogue   // team_setup.go; nil reads offerings from the knowledge base only
	// accountFacts reads the account record for business facts (business_facts.go); nil skips it.
	accountFacts AccountSource
	// pageReader is the crawler fallback for business facts (business_facts.go); nil skips it.
	pageReader PageReader
	// platformSlug is the platform owner's tenant (PLATFORM_TENANT): the only exempt tenant.
	platformSlug string
	// demoPlan is the free plan a demo tenant follows in the AI work (DEMO_PLAN_CODE).
	demoPlan string
}

// NewService builds the service; subs may be nil (no plan information).
func NewService(db *ent.Client, subs *subscriptions.Client, logger *zap.Logger) *Service {
	return &Service{db: db, subs: subs, logger: logger.Named("marketingos")}
}

// WithPlatformTenant names the platform owner's tenant, the only one the OS context marks
// exempt (demo tenants are not; see OSContext.Exempt).
func (s *Service) WithPlatformTenant(slug string) *Service {
	s.platformSlug = strings.TrimSpace(slug)
	return s
}

// WithDemoPlan names the free plan a demo tenant follows in the AI work.
func (s *Service) WithDemoPlan(code string) *Service {
	s.demoPlan = strings.TrimSpace(code)
	return s
}

// GetBrandContext returns the stored context (empty when none is saved).
func (s *Service) GetBrandContext(ctx context.Context, tenantID uuid.UUID) (BrandContext, error) {
	var bc BrandContext
	t, err := s.db.Tenant.Get(ctx, tenantID)
	if err != nil {
		return bc, err
	}
	if raw, ok := t.Metadata[MetadataKey]; ok {
		b, _ := json.Marshal(raw)
		_ = json.Unmarshal(b, &bc)
	}
	bc.BannedClaims = shareddefaults.OwnClaims(t.BusinessType, bc.BannedClaims)
	bc.SharedBannedClaims = shareddefaults.SharedClaims(t.BusinessType)
	return bc, nil
}

// SaveBrandContext validates and stores the context. Only its own key is
// written (one atomic merge), so a profile or onboarding save running at the
// same time keeps its keys.
func (s *Service) SaveBrandContext(ctx context.Context, tenantID uuid.UUID, bc BrandContext) (BrandContext, error) {
	t, err := s.db.Tenant.Get(ctx, tenantID)
	if err != nil {
		return bc, err
	}
	// The shared claims are inherited, so only the business's own are stored.
	bc.SharedBannedClaims = nil
	bc.BannedClaims = shareddefaults.OwnClaims(t.BusinessType, bc.BannedClaims)
	if err := bc.Validate(); err != nil {
		return bc, err
	}
	b, _ := json.Marshal(bc)
	var asMap map[string]any
	_ = json.Unmarshal(b, &asMap)
	if err := s.setKeys(ctx, tenantID, map[string]any{MetadataKey: asMap}); err != nil {
		return bc, err
	}
	bc.SharedBannedClaims = shareddefaults.SharedClaims(t.BusinessType)
	return bc, nil
}

// EffectiveContext is the stored brand context (empty when none) with the banned claims that
// apply: the shared ones and the business's own.
func EffectiveContext(t *ent.Tenant) map[string]any {
	stored, _ := t.Metadata[MetadataKey].(map[string]any)
	out := make(map[string]any, len(stored)+1)
	for k, v := range stored {
		out[k] = v
	}
	out["banned_claims"] = shareddefaults.Union(shareddefaults.SharedClaims(t.BusinessType), readList(stored["banned_claims"]))
	return out
}

// setKeys merges tenant metadata keys; an unknown tenant is a not-found error.
func (s *Service) setKeys(ctx context.Context, tenantID uuid.UUID, values map[string]any) error {
	err := metastore.SetKeys(ctx, s.db, tenantID, values)
	if err != nil {
		if ok, xerr := s.db.Tenant.Query().Where(tenant.ID(tenantID)).Exist(ctx); xerr == nil && !ok {
			return &ent.NotFoundError{}
		}
	}
	return err
}

// GetSettings reads the tenant's Marketing OS switches.
func (s *Service) GetSettings(ctx context.Context, tenantID uuid.UUID) (Settings, error) {
	t, err := s.db.Tenant.Get(ctx, tenantID)
	if err != nil {
		return Settings{}, err
	}
	on, ok := t.Metadata[DailyBriefKey].(bool)
	auto, _ := t.Metadata[InboxAutoReplyKey].(bool)
	text, _ := t.Metadata[InboxReplyTextKey].(string)
	var competitors []string
	if list, ok := t.Metadata[CompetitorsKey].([]any); ok {
		for _, c := range list {
			if s, ok := c.(string); ok {
				competitors = append(competitors, s)
			}
		}
	}
	out := Settings{DailyBrief: !ok || on, InboxAutoReply: auto, InboxReplyText: text, Competitors: competitors,
		ReferenceAds: readReferenceAds(t.Metadata), CustomerComplaints: readStrings(t.Metadata, CustomerComplaintsKey)}
	out.ContactEnrichment, out.ContactEnrichmentWeb = ContactEnrichmentSwitches(t.Metadata)
	readInboxOverrides(t.Metadata, &out)
	readBoostOTPThreshold(t.Metadata, &out)
	return out, nil
}

// SaveSettings stores the switches, merging only their keys.
func (s *Service) SaveSettings(ctx context.Context, tenantID uuid.UUID, in Settings) (Settings, error) {
	in.InboxReplyText = strings.TrimSpace(in.InboxReplyText)
	if utf8.RuneCountInString(in.InboxReplyText) > MaxInboxReplyText {
		in.InboxReplyText = string([]rune(in.InboxReplyText)[:MaxInboxReplyText])
	}
	// The switch cannot be on without a message to send.
	if in.InboxReplyText == "" {
		in.InboxAutoReply = false
	}
	seen := map[string]bool{}
	clean := []string{}
	for _, c := range in.Competitors {
		c = strings.TrimSpace(c)
		if c != "" && len(c) <= 120 && !seen[strings.ToLower(c)] && len(clean) < MaxCompetitors {
			seen[strings.ToLower(c)] = true
			clean = append(clean, c)
		}
	}
	in.Competitors = clean
	refs, err := cleanReferenceAds(in.ReferenceAds)
	if err != nil {
		return in, fmt.Errorf("%w: %v", ErrInvalidSettings, err)
	}
	complaints, err := cleanComplaints(in.CustomerComplaints)
	if err != nil {
		return in, fmt.Errorf("%w: %v", ErrInvalidSettings, err)
	}
	in.ReferenceAds, in.CustomerComplaints = refs, complaints
	if err := cleanInboxOverrides(&in); err != nil {
		return in, fmt.Errorf("%w: %v", ErrInvalidSettings, err)
	}
	if err := cleanBoostOTPThreshold(in.BoostOTPThresholdKES); err != nil {
		return in, fmt.Errorf("%w: %v", ErrInvalidSettings, err)
	}
	// The website lookup only runs inside the enricher.
	if !in.ContactEnrichment {
		in.ContactEnrichmentWeb = false
	}
	values := map[string]any{
		DailyBriefKey: in.DailyBrief, InboxAutoReplyKey: in.InboxAutoReply, InboxReplyTextKey: in.InboxReplyText,
		CompetitorsKey: in.Competitors, ReferenceAdsKey: in.ReferenceAds, CustomerComplaintsKey: in.CustomerComplaints,
		ContactEnrichmentKey: in.ContactEnrichment, ContactEnrichmentWebKey: in.ContactEnrichmentWeb,
		BoostOTPThresholdKey: boostOTPThresholdValue(in),
	}
	for k, v := range inboxOverrideValues(in) {
		values[k] = v
	}
	if err := s.queueProfileClear(ctx, tenantID, in, values); err != nil {
		return in, err
	}
	return in, s.setKeys(ctx, tenantID, values)
}

// queueProfileClear adds the clear marker to values when a save switches the contact enricher
// off (every stored profile goes) or only its website lookup off (the company descriptions go).
// The worker does the clear in the background (worker.ContactEnricherSweep). Switching the
// enricher back on before it ran cancels it; the daily pass then rebuilds the profiles.
func (s *Service) queueProfileClear(ctx context.Context, tenantID uuid.UUID, in Settings, values map[string]any) error {
	t, err := s.db.Tenant.Get(ctx, tenantID)
	if err != nil {
		return err
	}
	if v, set := profileClear(t.Metadata, in); set {
		values[ContactEnrichmentClearKey] = v
	}
	return nil
}

// profileClear decides the clear marker for a save: the new value (nil removes it) and whether
// to write it at all.
func profileClear(stored map[string]any, in Settings) (any, bool) {
	wasOn, wasWeb := ContactEnrichmentSwitches(stored)
	pending, _ := stored[ContactEnrichmentClearKey].(string)
	switch {
	case wasOn && !in.ContactEnrichment:
		return ClearAllProfiles, true
	case !in.ContactEnrichment:
		return nil, false // already off: keep whatever clear is still pending
	case wasWeb && !in.ContactEnrichmentWeb && pending != ClearAllProfiles:
		return ClearCompanySummaries, true
	case pending == ClearAllProfiles || (pending == ClearCompanySummaries && in.ContactEnrichmentWeb):
		return nil, true // switched back on before the clear ran
	}
	return nil, false
}

// overrideName is the name the owner gave a member, empty when they kept the
// default. Rows saved before metadata carried the name fall back to the row
// name unless it is the key-derived label.
func overrideName(a *ent.CustomAgent) string {
	if n, ok := a.Metadata["name"].(string); ok {
		return n
	}
	if a.Name == strings.ReplaceAll(str(a.Metadata["key"]), "_", " ") {
		return ""
	}
	return a.Name
}

// TeamOverrides returns the tenant's team member overrides.
func (s *Service) TeamOverrides(ctx context.Context, tenantID uuid.UUID) ([]MemberOverride, error) {
	rows, err := s.db.CustomAgent.Query().
		Where(customagent.TenantID(tenantID), func(sel *sql.Selector) {
			sel.Where(sqljson.ValueEQ(customagent.FieldMetadata, KindTeamMember, sqljson.Path("kind")))
		}).
		Limit(50). // the team has ten members; this only bounds bad data
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]MemberOverride, 0, len(rows))
	for _, a := range rows {
		o := MemberOverride{Key: str(a.Metadata["key"]), Name: overrideName(a), Disabled: !a.IsActive,
			ModelClass: str(a.Metadata["model_class"]), Instructions: str(a.Metadata["instructions"]), SetBy: str(a.Metadata["set_by"])}
		if o.Key != "" {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// UpsertTeamMember stores one override (rename, switch off, model class,
// extra instructions) as the tenant's CustomAgent row for that member.
func (s *Service) UpsertTeamMember(ctx context.Context, tenantID uuid.UUID, o MemberOverride) (MemberOverride, error) {
	o.Key = strings.TrimSpace(o.Key)
	o.Name = strings.TrimSpace(o.Name)
	o.Instructions = strings.TrimSpace(o.Instructions)
	if !memberKeyRe.MatchString(o.Key) {
		return o, fmt.Errorf("unknown member key")
	}
	if !modelClasses[o.ModelClass] {
		return o, fmt.Errorf("model_class must be nano, standard, premium or premium_plus")
	}
	if len(o.Name) > 128 || len(o.Instructions) > 600 {
		return o, fmt.Errorf("name is limited to 128 and instructions to 600 characters")
	}
	if o.Key == "manager" {
		o.Disabled = false // the manager is the entry point and cannot be switched off
	}
	name := o.Name
	if name == "" {
		name = strings.ReplaceAll(o.Key, "_", " ")
	}
	// The owner's chosen name lives in metadata; the row name is only a label, so
	// switching a member off never renames it to its key.
	meta := map[string]any{"kind": KindTeamMember, "key": o.Key, "name": o.Name, "model_class": o.ModelClass, "instructions": o.Instructions}
	if o.SetBy == SetByTeamSetup {
		meta["set_by"] = SetByTeamSetup
	}

	existing, err := s.db.CustomAgent.Query().
		Where(customagent.TenantID(tenantID), func(sel *sql.Selector) {
			sel.Where(sql.And(
				sqljson.ValueEQ(customagent.FieldMetadata, KindTeamMember, sqljson.Path("kind")),
				sqljson.ValueEQ(customagent.FieldMetadata, o.Key, sqljson.Path("key")),
			))
		}).
		First(ctx)
	switch {
	case ent.IsNotFound(err):
		_, err = s.db.CustomAgent.Create().SetTenantID(tenantID).SetName(name).SetTriggerType("manual").
			SetIsActive(!o.Disabled).SetMetadata(meta).SetAllowedTools([]string{}).Save(ctx)
	case err == nil:
		_, err = existing.Update().SetName(name).SetIsActive(!o.Disabled).SetMetadata(meta).Save(ctx)
	}
	if err != nil {
		return o, err
	}
	o.Name = name
	return o, nil
}

// offeringCategories are the knowledge base categories that hold sellable items.
var offeringCategories = []string{"product", "service", "course", "menu"}

// Context assembles the OS context for marketflow-ai.
func (s *Service) Context(ctx context.Context, tenantID uuid.UUID) (OSContext, error) {
	out := OSContext{}
	t, err := s.db.Tenant.Get(ctx, tenantID)
	if err != nil {
		return out, err
	}
	out.BusinessType = t.BusinessType
	out.BusinessTypeChosen = BusinessTypeChosen(t.Metadata)
	out.Exempt = s.platformSlug != "" && t.Slug == s.platformSlug
	// Always present, since the shared banned claims apply before a brand context is saved.
	if b, err := json.Marshal(EffectiveContext(t)); err == nil {
		out.MarketingContext = string(b)
	}
	if sig := marketSignals(t.Metadata); !sig.Empty() {
		out.MarketSignals = &sig
	}
	out.PaymentMethods, out.Glossary = s.glossaryFor(ctx, tenantID)
	titles, err := s.db.KnowledgeBaseEntry.Query().
		Where(knowledgebaseentry.TenantID(tenantID), knowledgebaseentry.CategoryIn(offeringCategories...)).
		Order(ent.Asc(knowledgebaseentry.FieldTitle)).
		Limit(200).
		Select(knowledgebaseentry.FieldTitle).
		Strings(ctx)
	if err != nil {
		return out, err
	}
	seen := map[string]bool{}
	for _, list := range [][]string{titles, brandOffers(t.Metadata)} {
		for _, v := range list {
			v = strings.TrimSpace(v)
			if v != "" && !seen[strings.ToLower(v)] {
				seen[strings.ToLower(v)] = true
				out.Offerings = append(out.Offerings, v)
			}
		}
	}
	if out.Members, err = s.TeamOverrides(ctx, tenantID); err != nil {
		return out, err
	}
	if s.subs != nil {
		if ent := s.subs.GetEntitlements(ctx, tenantID.String()); ent != nil {
			status := strings.ToUpper(ent.Status)
			out.HasPlan = status == "ACTIVE" || status == "TRIAL" || ent.IsDemoBypass
			out.Features = make(map[string]bool, len(ent.Features))
			for _, f := range ent.Features {
				out.Features[f] = true
			}
			if n := ent.Limits["chat_turns_daily"]; n > 0 {
				out.ChatTurnsDaily = n
			}
			// A demo tenant has no plan of its own; its password is public, so in the AI work it
			// gets exactly the free plan's features and limits (owner, 2026-11-12). The platform
			// owner's own tenant is Exempt instead and keeps every level.
			if ent.IsDemoBypass && !out.Exempt && s.demoPlan != "" {
				if p := s.subs.GetPlan(ctx, s.demoPlan); p != nil {
					out.Features = make(map[string]bool, len(p.Features))
					for _, f := range p.Features {
						out.Features[f] = true
					}
					if n := p.Limits["chat_turns_daily"]; n > 0 {
						out.ChatTurnsDaily = n
					}
				}
			}
		}
	}
	return out, nil
}

func brandOffers(meta map[string]any) []string {
	raw, ok := meta[MetadataKey].(map[string]any)
	if !ok {
		return nil
	}
	list, _ := raw["offers"].([]any)
	out := make([]string, 0, len(list))
	for _, v := range list {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
