// Package onboarding guides a new tenant through setting up MarketFlow: a checklist computed
// from data that already exists, and a suggested brand context built from what the business
// already has in other services. It owns no business data: the profile and brand context are
// saved through their own endpoints, and catalogue and identity data stay with their owners
// (inventory-api, pos-api, auth-api, codevertex-website), read here only as snapshots.
package onboarding

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/ent/funnel"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/ent/marketflowuser"
	"github.com/bengobox/marketflow-api/internal/ent/testimonial"
	"github.com/bengobox/marketflow-api/internal/modules/creative"
	"github.com/bengobox/marketflow-api/internal/modules/marketingos"
	"github.com/bengobox/marketflow-api/internal/modules/tenant"
	"github.com/bengobox/marketflow-api/internal/modules/tenant/metastore"
)

// BrandStore reads the saved brand context (marketingos.Service is the only writer).
type BrandStore interface {
	GetBrandContext(ctx context.Context, tenantID uuid.UUID) (marketingos.BrandContext, error)
}

// Service builds the checklist and the brand context draft.
type Service struct {
	db           *ent.Client
	brand        BrandStore
	auth         *tenant.Syncer
	top          *creative.TopOfferings
	website      *WebsiteCatalog
	platformSlug string
	logger       *zap.Logger
}

// NewService wires the sources; auth, top and website may be nil (that source is skipped).
func NewService(db *ent.Client, brand BrandStore, auth *tenant.Syncer, top *creative.TopOfferings, website *WebsiteCatalog, platformSlug string, logger *zap.Logger) *Service {
	return &Service{db: db, brand: brand, auth: auth, top: top, website: website, platformSlug: platformSlug, logger: logger.Named("onboarding")}
}

// draftBudget bounds the whole draft; each outside source has its own shorter timeout inside it.
const draftBudget = 4 * time.Second

// Step is one item of the setup checklist.
type Step struct {
	Key      string `json:"key"`
	Done     bool   `json:"done"`
	Optional bool   `json:"optional,omitempty"`
}

// Checklist is the tenant's setup progress. Done and Total count required steps only.
type Checklist struct {
	Steps     []Step `json:"steps"`
	Done      int    `json:"done"`
	Total     int    `json:"total"`
	Complete  bool   `json:"complete"`
	Dismissed bool   `json:"dismissed"`
}

// Checklist computes progress from existing data: each step is one indexed existence check.
func (s *Service) Checklist(ctx context.Context, tenantID uuid.UUID) (Checklist, error) {
	t, err := s.db.Tenant.Get(ctx, tenantID)
	if err != nil {
		return Checklist{}, fmt.Errorf("load tenant: %w", err)
	}
	meta := t.Metadata
	bc, _ := s.brand.GetBrandContext(ctx, tenantID)

	hasChannel := t.WhatsappNumber != nil && strings.TrimSpace(*t.WhatsappNumber) != ""
	if !hasChannel {
		hasChannel, _ = s.db.ConnectedAccount.Query().
			Where(connectedaccount.TenantID(tenantID), connectedaccount.Status("active")).Exist(ctx)
	}
	hasCapture, _ := s.db.Funnel.Query().Where(funnel.TenantID(tenantID), funnel.Status("published")).Exist(ctx)
	if !hasCapture {
		hasCapture, _ = s.db.Lead.Query().Where(lead.TenantID(tenantID)).Exist(ctx)
	}
	team, _ := s.db.MarketflowUser.Query().Where(marketflowuser.TenantID(tenantID)).Limit(2).Count(ctx)

	steps := []Step{
		{Key: "profile", Done: str(meta, "tagline") != "" && str(meta, "bio") != "" && (hasChannel || str(meta, "contact_email") != "")},
		{Key: "offerings", Done: listLen(meta, "offerings") > 0},
		{Key: "brand_context", Done: strings.TrimSpace(bc.Positioning) != "" && len(bc.Audience) > 0 && len(bc.Offers) > 0},
		{Key: "channels", Done: hasChannel},
		{Key: "lead_capture", Done: hasCapture},
		{Key: "team", Done: team > 1, Optional: true},
	}
	out := Checklist{Steps: steps}
	for _, st := range steps {
		if st.Optional {
			continue
		}
		out.Total++
		if st.Done {
			out.Done++
		}
	}
	out.Complete = out.Done == out.Total
	if state, ok := meta[tenant.OnboardingMetadataKey].(map[string]any); ok {
		_, out.Dismissed = state["dismissed_at"].(string)
	}
	return out, nil
}

// SetDismissed hides (or brings back) the setup prompt on the dashboard.
func (s *Service) SetDismissed(ctx context.Context, tenantID uuid.UUID, dismissed bool) error {
	state := map[string]any{}
	if dismissed {
		state["dismissed_at"] = time.Now().UTC().Format(time.RFC3339)
	}
	return metastore.SetKeys(ctx, s.db, tenantID, map[string]any{tenant.OnboardingMetadataKey: state})
}

// ProfileDefaults are values for the profile form taken from auth-api, used only where the
// MarketFlow profile has nothing yet.
type ProfileDefaults struct {
	ContactEmail string `json:"contact_email,omitempty"`
	WhatsApp     string `json:"whatsapp_number,omitempty"`
	LogoURL      string `json:"logo_url,omitempty"`
	Website      string `json:"website,omitempty"`
	BrandColor   string `json:"brand_primary_color,omitempty"`
}

// Suggestion is a catalogue entry the owner can add to the public profile's offerings.
type Suggestion struct {
	Name         string  `json:"name"`
	Description  string  `json:"description,omitempty"`
	PriceLabel   string  `json:"price_label,omitempty"`
	ImageURL     string  `json:"image_url,omitempty"`
	URL          string  `json:"url,omitempty"`
	Source       string  `json:"source"` // pos | inventory | website | course
	QuantitySold float64 `json:"quantity_sold,omitempty"`
}

// Draft is a brand context the owner reviews before saving. Suggested keeps every field the
// owner already filled and only fills the empty ones.
type Draft struct {
	Current   marketingos.BrandContext `json:"current"`
	Suggested marketingos.BrandContext `json:"suggested"`
	Offerings []Suggestion             `json:"offerings"`
	Profile   ProfileDefaults          `json:"profile"`
	Sources   []string                 `json:"sources"`
}

// Draft gathers what the business already has elsewhere. Sources run in parallel inside
// draftBudget; one that fails or times out is left out, never fails the whole draft.
func (s *Service) Draft(ctx context.Context, tenantID uuid.UUID) (Draft, error) {
	t, err := s.db.Tenant.Get(ctx, tenantID)
	if err != nil {
		return Draft{}, fmt.Errorf("load tenant: %w", err)
	}
	current, err := s.brand.GetBrandContext(ctx, tenantID)
	if err != nil {
		return Draft{}, err
	}
	testimonials, _ := s.db.Testimonial.Query().
		Where(testimonial.TenantIDEQ(tenantID), testimonial.IsPublishedEQ(true)).
		Order(ent.Desc(testimonial.FieldRating), ent.Asc(testimonial.FieldDisplayOrder)).
		Limit(5).All(ctx)

	ctx, cancel := context.WithTimeout(ctx, draftBudget)
	defer cancel()
	var (
		wg      sync.WaitGroup
		auth    *tenant.AuthProfile
		top     []creative.TopOffering
		catalog Catalog
		webOK   bool
	)
	if s.auth != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p, err := s.auth.AuthProfile(ctx, t.Slug); err == nil {
				auth = p
			}
		}()
	}
	isPlatform := s.platformSlug != "" && t.Slug == s.platformSlug
	if isPlatform && s.website != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c, err := s.website.Load(ctx); err == nil {
				catalog, webOK = c, true
			}
		}()
	}
	if !isPlatform && s.top != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if items, err := s.top.Top(ctx, tenantID, 20); err == nil {
				top = items
			}
		}()
	}
	wg.Wait()

	return compose(composeInput{
		tenant: t, current: current, auth: auth, top: top,
		catalog: catalog, website: webOK, testimonials: testimonials,
	}), nil
}

func str(meta map[string]any, key string) string {
	s, _ := meta[key].(string)
	return strings.TrimSpace(s)
}

func listLen(meta map[string]any, key string) int {
	l, _ := meta[key].([]any)
	return len(l)
}
