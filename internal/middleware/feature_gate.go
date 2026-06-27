package middleware

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"go.uber.org/zap"
)

// FeatureGate checks subscription-level feature entitlements before allowing
// an action. Calls subscriptions-api `/features/{code}/check` (Redis-cached
// at 60s TTL on their side) and returns an error when the tenant has exceeded
// their plan limit. Used as a guard BEFORE creating campaigns, sending chats,
// or capturing leads.
type FeatureGate struct {
	subscriptionsURL string
	apiKey           string
	http             *http.Client
	logger           *zap.Logger
}

func NewFeatureGate(subscriptionsURL, apiKey string, logger *zap.Logger) *FeatureGate {
	return &FeatureGate{
		subscriptionsURL: subscriptionsURL,
		apiKey:           apiKey,
		http:             &http.Client{Timeout: 5 * time.Second},
		logger:           logger.Named("feature_gate"),
	}
}

// FeatureCheckResult mirrors the subscriptions-api response.
type FeatureCheckResult struct {
	Allowed    bool    `json:"allowed"`
	LimitValue float64 `json:"limit_value"`
	CurrentUse float64 `json:"current_usage"`
	Source     string  `json:"source"`
}

// Check returns (allowed, result, error). If the subscriptions service is
// unreachable the gate fails OPEN (allowed=true) to avoid blocking the
// entire platform when a downstream is down.
func (fg *FeatureGate) Check(tenantID, featureCode string) (bool, *FeatureCheckResult) {
	if fg.subscriptionsURL == "" || tenantID == "" {
		return true, nil
	}
	url := fmt.Sprintf("%s/api/v1/features/%s/check", fg.subscriptionsURL, featureCode)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("X-Tenant-ID", tenantID)
	if fg.apiKey != "" {
		req.Header.Set("X-API-Key", fg.apiKey)
	}
	resp, err := fg.http.Do(req)
	if err != nil {
		fg.logger.Debug("feature check failed, failing open", zap.String("feature", featureCode), zap.Error(err))
		return true, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		fg.logger.Debug("feature check non-200, failing open", zap.String("feature", featureCode), zap.Int("status", resp.StatusCode))
		return true, nil
	}
	var result FeatureCheckResult
	if json.Unmarshal(body, &result) != nil {
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
			if claims, ok := authclient.ClaimsFromContext(r.Context()); ok && claims != nil && claims.IsGatingExempt() {
				next.ServeHTTP(w, r)
				return
			}
			tenantID := r.Header.Get("X-Tenant-ID")
			allowed, result := fg.Check(tenantID, featureCode)
			if !allowed {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusPaymentRequired)
				msg := upgradeMsg
				if result != nil {
					msg = fmt.Sprintf("%s (current: %.0f, limit: %.0f)", upgradeMsg, result.CurrentUse, result.LimitValue)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"code":    "feature_limit_exceeded",
					"error":   msg,
					"feature": featureCode,
					"upgrade": "Upgrade your plan at /settings/billing",
				})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
