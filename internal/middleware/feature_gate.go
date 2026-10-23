package middleware

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	serviceclient "github.com/Bengo-Hub/shared-service-client"
	"github.com/sony/gobreaker"
	"go.uber.org/zap"
)

// FeatureGate checks subscription-level feature entitlements before allowing
// an action. Calls subscriptions-api `/features/{code}/check` (Redis-cached
// at 60s TTL on their side) and returns an error when the tenant has exceeded
// their plan limit. Used as a guard BEFORE creating campaigns, sending chats,
// or capturing leads.
//
// The check sits on the request path, so it must never make a request feel stuck: one check
// gets featureCheckBudget in total (retries included), and the shared client's circuit breaker
// opens after a few consecutive failures, after which checks fail open at once instead of each
// waiting for a service that is down.
type FeatureGate struct {
	client *serviceclient.Client
	apiKey string
	logger *zap.Logger
}

const featureCheckBudget = 2 * time.Second

func NewFeatureGate(subscriptionsURL, apiKey string, logger *zap.Logger) *FeatureGate {
	logger = logger.Named("feature_gate")
	fg := &FeatureGate{apiKey: apiKey, logger: logger}
	if subscriptionsURL == "" {
		return fg
	}
	cfg := serviceclient.DefaultConfig(strings.TrimSuffix(subscriptionsURL, "/"), "subscriptions-api", logger)
	cfg.Timeout = 1500 * time.Millisecond
	cfg.MaxElapsedTime = featureCheckBudget
	cfg.MaxInterval = 500 * time.Millisecond
	cfg.TimeoutCB = 30 * time.Second
	cfg.ReadyToTrip = func(c gobreaker.Counts) bool { return c.ConsecutiveFailures >= 3 }
	fg.client = serviceclient.New(cfg)
	return fg
}

// FeatureCheckResult mirrors the subscriptions-api response.
type FeatureCheckResult struct {
	Allowed    bool    `json:"allowed"`
	LimitValue float64 `json:"limit_value"`
	CurrentUse float64 `json:"current_usage"`
	Source     string  `json:"source"`
}

// Check returns (allowed, result). If the subscriptions service is unreachable, slow or
// answers badly, the gate fails OPEN (allowed=true) to avoid blocking the entire platform
// when a downstream is down.
func (fg *FeatureGate) Check(ctx context.Context, tenantID, featureCode string) (bool, *FeatureCheckResult) {
	if fg.client == nil || tenantID == "" {
		return true, nil
	}
	ctx, cancel := context.WithTimeout(ctx, featureCheckBudget)
	defer cancel()
	headers := map[string]string{"X-Tenant-ID": tenantID}
	if fg.apiKey != "" {
		headers["X-API-Key"] = fg.apiKey
	}
	resp, err := fg.client.Get(ctx, "/api/v1/features/"+url.PathEscape(featureCode)+"/check", headers)
	if err != nil {
		fg.logger.Debug("feature check failed, failing open", zap.String("feature", featureCode), zap.Error(err))
		return true, nil
	}
	if resp.StatusCode != http.StatusOK {
		fg.logger.Debug("feature check non-200, failing open", zap.String("feature", featureCode), zap.Int("status", resp.StatusCode))
		return true, nil
	}
	var result FeatureCheckResult
	if json.Unmarshal(resp.Body, &result) != nil {
		return true, nil
	}
	return result.Allowed, &result
}

// RequireFeature returns an HTTP middleware that blocks the request with 402
// when the tenant has exceeded the given feature limit.
func (fg *FeatureGate) RequireFeature(featureCode, upgradeMsg string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Gating-exempt tenants (platform owner / demo / service-charge / sub-exempt)
			// bypass usage gating entirely — uniform with the rest of the fleet's
			// claims.IsGatingExempt() funnel — and we skip the S2S round-trip for them.
			claims, ok := authclient.ClaimsFromContext(r.Context())
			if ok && claims != nil && claims.IsGatingExempt() {
				next.ServeHTTP(w, r)
				return
			}
			// The quota is the signed-in tenant's: a header naming another tenant must not
			// spend (or dodge) someone else's limit. Platform service keys act for the tenant
			// in the header by design, and calls without claims keep the header too.
			tenantID := r.Header.Get("X-Tenant-ID")
			if ok && claims != nil && !claims.IsService && claims.TenantID != "" {
				tenantID = claims.TenantID
			}
			allowed, result := fg.Check(r.Context(), tenantID, featureCode)
			if !allowed {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusPaymentRequired)
				msg := upgradeMsg
				if result != nil {
					msg = fmt.Sprintf("%s (current: %.0f, limit: %.0f)", upgradeMsg, result.CurrentUse, result.LimitValue)
				}
				// This gate is a metered usage-limit check (S2S current-vs-limit), so it uses the
				// canonical usage code the frontends' isSubscriptionError() recognizes — was the
				// non-standard "feature_limit_exceeded", which no frontend matched.
				_ = json.NewEncoder(w).Encode(map[string]any{
					"code":    "usage_limit_exceeded",
					"error":   msg,
					"feature": featureCode,
					"upgrade": true,
				})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
