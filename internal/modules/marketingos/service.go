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

	"entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqljson"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/customagent"
	"github.com/bengobox/marketflow-api/internal/ent/knowledgebaseentry"
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

// Settings are the tenant's Marketing OS switches.
type Settings struct {
	DailyBrief bool `json:"daily_brief"`
}

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
	return nil
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
}

var (
	memberKeyRe  = regexp.MustCompile(`^[a-z][a-z_]{2,40}$`)
	modelClasses = map[string]bool{"": true, "nano": true, "standard": true, "premium": true, "premium_plus": true}
)

// OSContext is what marketflow-ai reads for each request.
type OSContext struct {
	BusinessType     string           `json:"business_type"`
	MarketingContext string           `json:"marketing_context"`
	Offerings        []string         `json:"offerings"`
	Members          []MemberOverride `json:"members"`
	Features         map[string]bool  `json:"features,omitempty"`
	HasPlan          bool             `json:"has_plan"`
}

// Service serves the Marketing OS data.
type Service struct {
	db     *ent.Client
	subs   *subscriptions.Client
	logger *zap.Logger
}

// NewService builds the service; subs may be nil (no plan information).
func NewService(db *ent.Client, subs *subscriptions.Client, logger *zap.Logger) *Service {
	return &Service{db: db, subs: subs, logger: logger.Named("marketingos")}
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
	return bc, nil
}

// SaveBrandContext validates and stores the context, keeping the rest of the
// tenant metadata untouched.
func (s *Service) SaveBrandContext(ctx context.Context, tenantID uuid.UUID, bc BrandContext) (BrandContext, error) {
	if err := bc.Validate(); err != nil {
		return bc, err
	}
	t, err := s.db.Tenant.Get(ctx, tenantID)
	if err != nil {
		return bc, err
	}
	meta := map[string]any{}
	for k, v := range t.Metadata {
		meta[k] = v
	}
	b, _ := json.Marshal(bc)
	var asMap map[string]any
	_ = json.Unmarshal(b, &asMap)
	meta[MetadataKey] = asMap
	if _, err := t.Update().SetMetadata(meta).Save(ctx); err != nil {
		return bc, err
	}
	return bc, nil
}

// GetSettings reads the tenant's Marketing OS switches.
func (s *Service) GetSettings(ctx context.Context, tenantID uuid.UUID) (Settings, error) {
	t, err := s.db.Tenant.Get(ctx, tenantID)
	if err != nil {
		return Settings{}, err
	}
	on, ok := t.Metadata[DailyBriefKey].(bool)
	return Settings{DailyBrief: !ok || on}, nil
}

// SaveSettings stores the switches, keeping the rest of the tenant metadata.
func (s *Service) SaveSettings(ctx context.Context, tenantID uuid.UUID, in Settings) (Settings, error) {
	t, err := s.db.Tenant.Get(ctx, tenantID)
	if err != nil {
		return in, err
	}
	meta := map[string]any{}
	for k, v := range t.Metadata {
		meta[k] = v
	}
	meta[DailyBriefKey] = in.DailyBrief
	if _, err := t.Update().SetMetadata(meta).Save(ctx); err != nil {
		return in, err
	}
	return in, nil
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
			ModelClass: str(a.Metadata["model_class"]), Instructions: str(a.Metadata["instructions"])}
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
	if raw, ok := t.Metadata[MetadataKey]; ok {
		b, _ := json.Marshal(raw)
		out.MarketingContext = string(b)
	}
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
