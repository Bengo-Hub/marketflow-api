package admin

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent/providersetting"
	"github.com/bengobox/marketflow-api/internal/httpapi"
)

// BillingURLKey is the ProviderSetting key that overrides a provider's billing page. The platform
// pays the AI and media providers directly, so a platform alert about a provider's balance links to
// that provider's own billing page; this is the one place that says where that page is.
const BillingURLKey = "billing_url"

// defaultBillingURLs are each provider's own billing or top-up page, used when its card has no
// billing_url. Keyed by ProviderSetting.provider_name. Any other provider gets a billing page by
// setting billing_url on its card.
var defaultBillingURLs = map[string]string{
	// Media (marketflow-ai's balance watcher reads these three).
	"fal":        "https://fal.ai/dashboard/billing",
	"higgsfield": "https://console.higgsfield.ai",
	"elevenlabs": "https://elevenlabs.io/app/subscription",
	// AI text models.
	"openai":     "https://platform.openai.com/settings/organization/billing/overview",
	"anthropic":  "https://console.anthropic.com/settings/billing",
	"groq":       "https://console.groq.com/settings/billing",
	"openrouter": "https://openrouter.ai/settings/credits",
}

// DefaultBillingURL is the provider's own billing page, or "" for a provider without one.
func DefaultBillingURL(provider string) string {
	return defaultBillingURLs[strings.ToLower(strings.TrimSpace(provider))]
}

// ValidBillingURL reports whether v is an absolute https link (the only kind an alert may carry).
func ValidBillingURL(v string) bool {
	u, err := url.Parse(strings.TrimSpace(v))
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil
}

// BillingURL is the billing page for a provider: its card's billing_url when that is a valid
// https link, else the default.
func BillingURL(provider, configured string) string {
	if ValidBillingURL(configured) {
		return strings.TrimSpace(configured)
	}
	return DefaultBillingURL(provider)
}

var providerNamePattern = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)

// configuredBillingURL reads a provider's billing_url from the platform settings (any type).
func (h *PlatformProviderHandler) configuredBillingURL(ctx context.Context, provider string) string {
	if h.client == nil {
		return ""
	}
	row, err := h.client.ProviderSetting.Query().
		Where(
			providersetting.TenantIDEQ(platformTenantID),
			providersetting.ProviderNameEQ(provider),
			providersetting.KeyEQ(BillingURLKey),
			providersetting.IsPlatformEQ(true),
			providersetting.IsActiveEQ(true),
		).
		Select(providersetting.FieldValue).
		First(ctx)
	if err != nil {
		return ""
	}
	return row.Value
}

// ProviderBilling handles GET /public/provider-billing/{provider}: the provider's billing page.
// The platform owner's WhatsApp balance alert has a button on MarketFlow's domain (Meta fixes a
// button's domain at approval), and that link forwards here to the provider's own page. The link
// is not a secret and only a known provider or a stored https link is ever returned, so this is
// no open redirect.
// @Summary A provider's billing page
// @Tags Public
// @Produce json
// @Param provider path string true "Provider name (fal, higgsfield, elevenlabs, openai, ...)"
// @Success 200 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Router /public/provider-billing/{provider} [get]
func (h *PlatformProviderHandler) ProviderBilling(w http.ResponseWriter, r *http.Request) {
	provider := strings.ToLower(chi.URLParam(r, "provider"))
	if !providerNamePattern.MatchString(provider) {
		httpapi.Error(w, http.StatusNotFound, "unknown_provider", "unknown provider", nil)
		return
	}
	link := BillingURL(provider, h.configuredBillingURL(r.Context(), provider))
	if link == "" {
		httpapi.Error(w, http.StatusNotFound, "unknown_provider", "no billing page for this provider", nil)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	httpapi.JSON(w, http.StatusOK, map[string]string{"provider": provider, "url": link})
}

// withBillingDefault adds the default billing_url to a platform provider's resolved settings
// when its card has none, so marketflow-ai's balance alerts carry the provider's own page.
func withBillingDefault(provider string, settings map[string]string) {
	if ValidBillingURL(settings[BillingURLKey]) {
		return
	}
	if d := DefaultBillingURL(provider); d != "" {
		settings[BillingURLKey] = d
	} else {
		delete(settings, BillingURLKey)
	}
}

// checkBillingURL rejects a billing_url that is set but not an https link.
func checkBillingURL(w http.ResponseWriter, settings map[string]string, logger *zap.Logger) bool {
	v := strings.TrimSpace(settings[BillingURLKey])
	if v == "" || ValidBillingURL(v) {
		return true
	}
	logger.Debug("billing_url refused", zap.String("value", v))
	httpapi.Error(w, http.StatusBadRequest, "invalid_billing_url", "billing_url must be an https link", nil)
	return false
}
