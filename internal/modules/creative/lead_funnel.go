package creative

import (
	"context"

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

// attachLeadFunnel creates the ad's lead page and keeps its id and slug on the brief. It never
// fails the ad: without a page the ad still works, and the failure is logged.
func (s *Service) attachLeadFunnel(ctx context.Context, b *ent.CreativeBrief) *ent.CreativeBrief {
	if s.leadFunnels == nil {
		return b
	}
	if id, _ := b.Metadata["funnel_id"].(string); id != "" {
		return b
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
		s.logger.Warn("lead funnel for a new ad not created", zap.String("brief_id", b.ID.String()), zap.Error(err))
		return b
	}
	meta := map[string]any{}
	for k, v := range b.Metadata {
		meta[k] = v
	}
	meta["funnel_id"], meta["funnel_slug"] = id.String(), slug
	updated, err := b.Update().SetMetadata(meta).Save(ctx)
	if err != nil {
		s.logger.Warn("lead funnel not linked to the ad", zap.String("brief_id", b.ID.String()), zap.Error(err))
		return b
	}
	return updated
}
