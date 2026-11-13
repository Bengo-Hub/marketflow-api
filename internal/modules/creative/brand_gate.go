package creative

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/modules/brandimages"
)

// ErrBrandImagesRequired refuses video ads and presenter content until the business has uploaded
// its brand images (owner, 2026-11-12). Posters and chat never need them.
var ErrBrandImagesRequired = errors.New("brand images required")

// WithBrandImagesMin sets how many brand images a video ad needs (brandimages.Config.Min).
func (s *Service) WithBrandImagesMin(n int) *Service {
	if n > 0 {
		s.brandImagesMin = n
	}
	return s
}

func (s *Service) minBrandImages() int {
	if s.brandImagesMin > 0 {
		return s.brandImagesMin
	}
	return brandimages.DefaultConfig().Min
}

// CheckBrandImages returns ErrBrandImagesRequired while the tenant has fewer brand images than
// the minimum. Only keys under the tenant's own prefix count.
func (s *Service) CheckBrandImages(ctx context.Context, tenantID uuid.UUID) error {
	have := 0
	if t, err := s.db.Tenant.Get(ctx, tenantID); err == nil {
		have = len(brandimages.Keys(t.Metadata, tenantID, 0))
	}
	if need := s.minBrandImages(); have < need {
		return fmt.Errorf("%w: %d of %d", ErrBrandImagesRequired, have, need)
	}
	return nil
}

// requireBrandImages answers 409 brand_images_required and returns false when the caller's tenant
// has too few brand images. Gating-exempt tenants (the shared claims check) are never blocked.
func (h *Handler) requireBrandImages(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID) bool {
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok && claims != nil && claims.IsGatingExempt() {
		return true
	}
	if err := h.svc.CheckBrandImages(r.Context(), tenantID); err != nil {
		need := h.svc.minBrandImages()
		httpapi.Error(w, http.StatusConflict, "brand_images_required",
			fmt.Sprintf("Video ads and presenter videos use pictures of your business. Upload at least %d brand images first.", need),
			map[string]any{"min": need, "upload_step": "brand_images"})
		return false
	}
	return true
}
