package creative

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/httpapi"
)

// The video plan gate (Sprint 15, gate G0): a video is made only when the tenant's plan includes
// the feature for that kind of video. The codes are marketflow-ai's model registry features
// (registry.go Feature), so the quote and the render agree on what a plan unlocks.
const (
	FeatureVideoTemplate         = "ai_video_template"
	FeatureVideoPresenter        = "ai_video_presenter"
	FeatureVideoPresenterPremium = "ai_video_presenter_premium"

	// planUpgradePath is where the owner changes plan, as the Autopilot plan gate answers.
	planUpgradePath = "/settings/billing"
)

// ErrNotInPlan is a video the tenant's plan does not include. Use errors.As with
// *NotInPlanError for the feature.
var ErrNotInPlan = errors.New("not in plan")

// NotInPlanError names the plan feature a refused video needs.
type NotInPlanError struct{ Feature string }

func (e *NotInPlanError) Error() string {
	return fmt.Sprintf("your plan does not include this kind of video (%s); upgrade your plan to make it", e.Feature)
}

func (e *NotInPlanError) Is(target error) bool { return target == ErrNotInPlan }

// VideoFeature is the plan feature a version kind and video kind need: motion posters and template
// video ads need ai_video_template, presenter ads ai_video_presenter, premium presenter ads
// ai_video_presenter_premium. Posters need no video feature ("").
func VideoFeature(versionKind, videoKind string) string {
	switch versionKind {
	case KindVideo:
		return FeatureVideoTemplate
	case KindVideoAd:
		switch videoKind {
		case VideoPresenter:
			return FeatureVideoPresenter
		case VideoPresenterPremium:
			return FeatureVideoPresenterPremium
		default:
			return FeatureVideoTemplate
		}
	}
	return ""
}

// WithFeatureCheck turns on the video plan gate. has answers whether a tenant's plan includes a
// feature; app.go passes the subscriptions client's ConsumerHasFeature, which already lets demo,
// platform owner and pay-as-you-go tenants through and fails open while subscriptions-api is down
// (the same contract as the FeatureGate middleware). Nil leaves the gate off (tests).
func (s *Service) WithFeatureCheck(has func(ctx context.Context, tenantID, feature string) bool) *Service {
	s.hasFeature = has
	return s
}

// CheckVideoFeature returns a *NotInPlanError when the tenant's plan does not include feature.
// A signed-in person whose claims are gating exempt (platform owner, demo, pay-as-you-go,
// subscription exempt: the shared IsGatingExempt rule) is never refused. A service key acting for
// a tenant carries the key's claims, not the tenant's, so the tenant's own plan decides then.
func (s *Service) CheckVideoFeature(ctx context.Context, tenantID uuid.UUID, feature string) error {
	if feature == "" || s.hasFeature == nil {
		return nil
	}
	if c, ok := authclient.ClaimsFromContext(ctx); ok && c != nil && !c.IsService && c.IsGatingExempt() {
		return nil
	}
	if s.hasFeature(ctx, tenantID.String(), feature) {
		return nil
	}
	return &NotInPlanError{Feature: feature}
}

// writeNotInPlan answers a refused video with the plan refusal shape the Autopilot plan gate uses:
// 403, a code, the message, and the feature and upgrade path in the details.
func writeNotInPlan(w http.ResponseWriter, err error) bool {
	var nip *NotInPlanError
	if !errors.As(err, &nip) {
		return false
	}
	httpapi.Error(w, http.StatusForbidden, "feature_not_in_plan", nip.Error(),
		map[string]any{"feature": nip.Feature, "upgrade_path": planUpgradePath, "upgrade": true})
	return true
}
