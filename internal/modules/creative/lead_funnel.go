package creative

import (
	"context"
	"errors"
	"fmt"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// LeadFunnelMaker publishes the lead page a new ad links to (funnels.Service.CreateForAd, wired
// in app). It returns the funnel's id and slug.
type LeadFunnelMaker func(ctx context.Context, tenantID, briefID uuid.UUID, title, offering, description, imageURL, language string) (uuid.UUID, string, error)

// WithLeadFunnels makes every new ad come with its own published lead page.
func (s *Service) WithLeadFunnels(m LeadFunnelMaker) *Service {
	s.leadFunnels = m
	return s
}

// ErrNoLeadFunnels means lead pages are not wired (tests, local development).
var ErrNoLeadFunnels = errors.New("lead pages are not available")

// attachLeadFunnel creates the ad's lead page and keeps its id and slug on the brief. It never
// fails the ad: without a page the ad still works, and the failure is logged.
func (s *Service) attachLeadFunnel(ctx context.Context, b *ent.CreativeBrief) *ent.CreativeBrief {
	out, err := s.ensureLeadFunnel(ctx, b)
	if err != nil {
		if !errors.Is(err, ErrNoLeadFunnels) {
			s.logger.Warn("lead funnel for a new ad not created", zap.String("brief_id", b.ID.String()), zap.Error(err))
		}
		return b
	}
	return out
}

// EnsureLeadFunnel gives an ad its lead page if it has none, for ads made before every ad got one.
// It is safe to repeat: an ad that has a page is returned as it is.
func (s *Service) EnsureLeadFunnel(ctx context.Context, tenantID, id uuid.UUID) (*ent.CreativeBrief, error) {
	b, err := s.brief(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	return s.ensureLeadFunnel(ctx, b)
}

func (s *Service) ensureLeadFunnel(ctx context.Context, b *ent.CreativeBrief) (*ent.CreativeBrief, error) {
	if id, _ := b.Metadata["funnel_id"].(string); id != "" {
		return b, nil
	}
	if s.leadFunnels == nil {
		return nil, ErrNoLeadFunnels
	}
	language := "en"
	if b.Language != "" {
		language = b.Language
	}
	image := ""
	if urls, ok := b.Offering["photo_urls"].([]any); ok && len(urls) > 0 {
		image, _ = urls[0].(string)
	}
	id, slug, err := s.leadFunnels(ctx, b.TenantID, b.ID, b.Title, str(b.Offering["name"]), str(b.Offering["description"]), image, language)
	if err != nil {
		return nil, fmt.Errorf("create lead page: %w", err)
	}
	meta := map[string]any{}
	for k, v := range b.Metadata {
		meta[k] = v
	}
	meta["funnel_id"], meta["funnel_slug"] = id.String(), slug
	updated, err := b.Update().SetMetadata(meta).Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("link lead page to the ad: %w", err)
	}
	return updated, nil
}
