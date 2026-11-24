package tenant

import (
	"net/http"
	"strings"

	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// tenantHeader reads X-Tenant-ID, writing the 400 itself when it is missing or bad.
func tenantHeader(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.Header.Get("X-Tenant-ID"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "X-Tenant-ID required", nil)
		return uuid.Nil, false
	}
	return id, true
}

// GetKESRate handles GET /api/v1/tenant/profile/kes-rate?currency=USD: how many KES one unit of
// the currency is worth, so a form can take an amount in the tenant's currency and hand a KES
// amount to a KES engine (the Creative Plan quote). The currency defaults to the tenant's own.
// The rate is fxrate.Rates.KESPer: treasury's rate, fresh from the shared cache or the last one
// treasury gave; never the configured default. No rate answers 503 no_rate, and the form keeps
// KES entry.
// @Summary KES per unit of the tenant's currency
// @Tags Tenant
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param currency query string false "ISO 4217 code; default the tenant's currency"
// @Success 200 {object} map[string]interface{}
// @Failure 503 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /tenant/profile/kes-rate [get]
func (h *Handler) GetKESRate(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantHeader(w, r)
	if !ok {
		return
	}
	cur := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("currency")))
	if cur == "" {
		cur = CurrencyOf(r.Context(), h.db, tenantID, PlatformCurrency)
	}
	if !IsSupportedCurrency(cur) {
		httpapi.Error(w, http.StatusBadRequest, "invalid_currency", "currency must be one of: "+strings.Join(SupportedCurrencies, ", "), nil)
		return
	}
	if cur == "KES" {
		httpapi.JSON(w, http.StatusOK, map[string]any{"currency": cur, "kes_per": 1.0, "source": "same"})
		return
	}
	if h.KESPer == nil {
		httpapi.Error(w, http.StatusServiceUnavailable, "no_rate", "exchange rates are not configured", nil)
		return
	}
	rate, err := h.KESPer(r.Context(), cur)
	if err != nil || rate <= 0 {
		h.logger.Warn("kes rate unavailable", zap.String("currency", cur), zap.Error(err))
		httpapi.Error(w, http.StatusServiceUnavailable, "no_rate", "no exchange rate for "+cur+" right now", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"currency": cur, "kes_per": rate, "source": "treasury"})
}
