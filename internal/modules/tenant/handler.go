package tenant

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	sharedcache "github.com/Bengo-Hub/cache"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/modules/audit"
	"github.com/bengobox/marketflow-api/internal/modules/marketingos"
	"github.com/bengobox/marketflow-api/internal/modules/tenant/metastore"
	"github.com/bengobox/marketflow-api/internal/platform/leadresponse"
	"github.com/bengobox/marketflow-api/internal/platform/runads"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Handler serves tenant profile settings (FBO branding, phone, bio, etc.).
type Handler struct {
	db     *ent.Client
	logger *zap.Logger
	// CanApprove tells whether the caller holds an approving role (owner or admin). Only they
	// may change the AI label (owner 2026-11-14); nil allows every caller.
	CanApprove func(ctx context.Context, tenantID uuid.UUID) bool
	// ProfileCache holds the public profile responses (Sprint 7 #58); a save drops the tenant's
	// entries. Nil when Redis is not configured.
	ProfileCache *sharedcache.Aside
	// KESPer is fxrate.Rates.KESPer (treasury's rate, cached or its last one), for GetKESRate.
	KESPer func(ctx context.Context, currency string) (float64, error)
	// PlatformLeadFilter is the platform's lead filter lists by kind (leads.PlatformLeadFilter,
	// MF_LEAD_FILTER_*), shown read only beside the tenant's own words.
	PlatformLeadFilter func() map[string][]string
}

func NewHandler(db *ent.Client, logger *zap.Logger) *Handler {
	return &Handler{db: db, logger: logger.Named("tenant.handler")}
}

type profileUpdateRequest struct {
	Name           *string        `json:"name,omitempty"`
	WhatsappNumber *string        `json:"whatsapp_number,omitempty"`
	FboDistribID   *string        `json:"fbo_distrib_id,omitempty"`
	FlpStoreCode   *string        `json:"flp_store_code,omitempty"`
	FlpLanguage    *string        `json:"flp_language,omitempty"`
	Country        *string        `json:"country,omitempty"`
	BusinessType   *string        `json:"business_type,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
}

// GetProfile handles GET /api/v1/tenant/profile.
// @Summary Get current tenant profile
// @Tags Tenant
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /tenant/profile [get]
func (h *Handler) GetProfile(w http.ResponseWriter, r *http.Request) {
	tenantID, err := uuid.Parse(r.Header.Get("X-Tenant-ID"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "X-Tenant-ID required", nil)
		return
	}
	t, err := h.db.Tenant.Get(r.Context(), tenantID)
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "not_found", "tenant not found", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, serializeTenant(t))
}

// UpdateProfile handles PATCH /api/v1/tenant/profile.
// @Summary Update tenant profile
// @Tags Tenant
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param request body profileUpdateRequest true "Profile fields to update"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /tenant/profile [patch]
func (h *Handler) UpdateProfile(w http.ResponseWriter, r *http.Request) {
	tenantID, err := uuid.Parse(r.Header.Get("X-Tenant-ID"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "X-Tenant-ID required", nil)
		return
	}
	t, err := h.db.Tenant.Get(r.Context(), tenantID)
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "not_found", "tenant not found", nil)
		return
	}
	var req profileUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
		return
	}

	if req.Metadata != nil {
		if err := ValidateOfferings(req.Metadata["offerings"]); err != nil {
			httpapi.Error(w, http.StatusBadRequest, "invalid_offerings", err.Error(), nil)
			return
		}
		if err := ValidateSalesSettings(req.Metadata); err != nil {
			httpapi.Error(w, http.StatusBadRequest, "invalid_sales_settings", err.Error(), nil)
			return
		}
		if err := ValidateAILabel(req.Metadata); err != nil {
			httpapi.Error(w, http.StatusBadRequest, "invalid_ai_label", err.Error(), nil)
			return
		}
		if err := ValidateReportSettings(req.Metadata); err != nil {
			httpapi.Error(w, http.StatusBadRequest, "invalid_report_settings", err.Error(), nil)
			return
		}
		if err := ValidateBusinessAddress(req.Metadata); err != nil {
			httpapi.Error(w, http.StatusBadRequest, "invalid_business_address", err.Error(), nil)
			return
		}
		if err := ValidateCurrencySetting(req.Metadata); err != nil {
			httpapi.Error(w, http.StatusBadRequest, "invalid_currency", err.Error(), nil)
			return
		}
		if err := ValidateLeadFilterSetting(req.Metadata); err != nil {
			httpapi.Error(w, http.StatusBadRequest, "invalid_lead_filter", err.Error(), nil)
			return
		}
		if err := leadresponse.Validate(req.Metadata); err != nil {
			httpapi.Error(w, http.StatusBadRequest, "invalid_lead_response", err.Error(), nil)
			return
		}
		if err := runads.Validate(req.Metadata); err != nil {
			httpapi.Error(w, http.StatusBadRequest, "invalid_run_ads", err.Error(), nil)
			return
		}
		// Run ads defaults (owner, 2026-11-24) size real ad spend, so only owners and admins
		// change them. A form that sends the stored value back unchanged is not a change.
		if v, ok := req.Metadata[runads.TenantKey]; ok {
			if runads.Changed(t.Metadata, v) {
				if h.CanApprove != nil && !h.CanApprove(r.Context(), tenantID) {
					httpapi.Error(w, http.StatusForbidden, "forbidden", "only the account owner or an admin can change the Run ads defaults", nil)
					return
				}
			} else {
				delete(req.Metadata, runads.TenantKey)
			}
		}
		// Lead response (owner, 2026-11-24) is changed by owners and admins only. A form that
		// sends the stored value back unchanged is not a change.
		if v, ok := req.Metadata[leadresponse.TenantKey]; ok {
			if leadresponse.Changed(t.Metadata, v) {
				if h.CanApprove != nil && !h.CanApprove(r.Context(), tenantID) {
					httpapi.Error(w, http.StatusForbidden, "forbidden", "only the account owner or an admin can change lead response settings", nil)
					return
				}
			} else {
				delete(req.Metadata, leadresponse.TenantKey)
			}
		}
		// The lead filter decides which submissions are dropped, so only owners and admins
		// change it. A form that sends the stored words back unchanged is not a change.
		if v, ok := req.Metadata[LeadFilterKey]; ok {
			if LeadFilterChanged(t.Metadata, v) {
				if h.CanApprove != nil && !h.CanApprove(r.Context(), tenantID) {
					httpapi.Error(w, http.StatusForbidden, "forbidden", "only the account owner or an admin can change the lead filter", nil)
					return
				}
			} else {
				delete(req.Metadata, LeadFilterKey)
			}
		}
		// Owner 2026-11-14: only owners and admins change the AI label. A form that sends the
		// stored value back unchanged is not a change.
		if v, ok := req.Metadata[AILabelKey]; ok {
			if AILabelChanged(t.Metadata, v) {
				if h.CanApprove != nil && !h.CanApprove(r.Context(), tenantID) {
					httpapi.Error(w, http.StatusForbidden, "forbidden", "only the account owner or an admin can change the AI label", nil)
					return
				}
			} else {
				delete(req.Metadata, AILabelKey)
			}
		}
	}

	if req.Name != nil {
		if err := ValidateTenantName(*req.Name); err != nil {
			httpapi.Error(w, http.StatusBadRequest, "invalid_name", err.Error(), nil)
			return
		}
	}

	// Columns and metadata keys change together or not at all. Metadata is merged in the
	// database (only the sent, non-reserved keys), never rewritten whole, so a concurrent
	// brand context or settings save is not lost.
	tx, err := h.db.Tx(r.Context())
	if err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "save_failed", "could not save the profile", nil)
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	upd := tx.Tenant.UpdateOneID(tenantID)
	if req.Name != nil && strings.TrimSpace(*req.Name) != "" {
		upd = upd.SetName(strings.TrimSpace(*req.Name))
	}
	if req.WhatsappNumber != nil {
		clean, err := ValidatePhoneRequired(*req.WhatsappNumber)
		if err != nil {
			httpapi.Error(w, http.StatusBadRequest, "invalid_phone", err.Error(), nil)
			return
		}
		if t.WhatsappNumber == nil || *t.WhatsappNumber != clean {
			upd = upd.SetWhatsappNumber(clean)
		}
	}
	if req.FboDistribID != nil {
		v := strings.TrimSpace(*req.FboDistribID)
		if t.FboDistribID == nil || *t.FboDistribID != v {
			upd = upd.SetFboDistribID(v)
		}
	}
	if req.FlpStoreCode != nil {
		upd = upd.SetFlpStoreCode(strings.TrimSpace(*req.FlpStoreCode))
	}
	if req.FlpLanguage != nil {
		upd = upd.SetFlpLanguage(strings.TrimSpace(*req.FlpLanguage))
	}
	if req.Country != nil {
		upd = upd.SetCountry(strings.TrimSpace(*req.Country))
	}
	chosenAt := ""
	if req.BusinessType != nil {
		bt, ok := NormalizeBusinessType(*req.BusinessType)
		if !ok {
			httpapi.Error(w, http.StatusBadRequest, "invalid_business_type", "business_type must be one of: "+strings.Join(businessTypeKeys(), ", "), nil)
			return
		}
		if !businessTypeChangeAllowed(t.BusinessType, bt, callerIsPlatformOwner(r)) {
			httpapi.Error(w, http.StatusForbidden, "business_type_admin_only", "Only a MarketFlow platform admin can switch a business to or from Forever Living. Contact support to change it.", nil)
			return
		}
		upd = upd.SetBusinessType(bt)
		// An explicit choice is final, "generic" included: chat auto-detect and widget personas
		// no longer replace it (owner, 2026-11-14).
		chosenAt = time.Now().UTC().Format(time.RFC3339)
	}

	if _, err := upd.Save(r.Context()); err != nil {
		// The WhatsApp number routes inbound messages to one business, so it is unique.
		if ent.IsConstraintError(err) && strings.Contains(err.Error(), "whatsapp_number") {
			httpapi.Error(w, http.StatusConflict, "whatsapp_number_taken", "this WhatsApp number is already linked to another business; use a different number or contact support", nil)
			return
		}
		h.logger.Error("save profile", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "save_failed", "could not save the profile", nil)
		return
	}
	patch := ProfileMetadataPatch(req.Metadata)
	if chosenAt != "" {
		patch[marketingos.BusinessTypeChosenKey] = chosenAt
	}
	if len(patch) > 0 {
		if err := metastore.SetKeys(r.Context(), tx.Client(), tenantID, patch); err != nil {
			h.logger.Error("save profile metadata", zap.Error(err))
			httpapi.Error(w, http.StatusInternalServerError, "save_failed", "could not save the profile", nil)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		h.logger.Error("commit profile", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "save_failed", "could not save the profile", nil)
		return
	}
	committed = true
	// The public page reads a cached copy (Sprint 7 #58); drop it under the old and new names so
	// the change shows at once.
	updated, err := h.db.Tenant.Get(r.Context(), tenantID)
	if h.ProfileCache != nil {
		keys := PublicProfileCacheKeys(t)
		if err == nil {
			keys = append(keys, PublicProfileCacheKeys(updated)...)
		}
		h.ProfileCache.Invalidate(r.Context(), keys...)
	}
	if err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "save_failed", "saved, but could not read the profile back", nil)
		return
	}
	changed := []string{}
	for k := range req.Metadata {
		changed = append(changed, "metadata."+k)
	}
	if req.Name != nil {
		changed = append(changed, "name")
	}
	if req.WhatsappNumber != nil {
		changed = append(changed, "whatsapp_number")
	}
	if req.FboDistribID != nil {
		changed = append(changed, "fbo_distrib_id")
	}
	if req.BusinessType != nil {
		changed = append(changed, "business_type")
	}
	audit.Tenant(r.Context(), h.db, h.logger, tenantID, "tenant.profile_updated", "tenant", &tenantID, map[string]any{"fields": changed})
	httpapi.JSON(w, http.StatusOK, serializeTenant(updated))
}

// ListBusinessTypes handles GET /api/v1/tenant/profile/business-types: the business types an owner
// can pick in onboarding and settings, each with a label and description (Sprint 7 #32).
// @Summary List business types
// @Tags Tenant
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /tenant/profile/business-types [get]
func (h *Handler) ListBusinessTypes(w http.ResponseWriter, r *http.Request) {
	httpapi.JSON(w, http.StatusOK, map[string]any{"business_types": BusinessTypes})
}

func businessTypeKeys() []string {
	keys := make([]string, 0, len(BusinessTypes))
	for _, t := range BusinessTypes {
		keys = append(keys, t.Key)
	}
	return keys
}

func serializeTenant(t *ent.Tenant) map[string]any {
	out := map[string]any{
		"id":            t.ID,
		"name":          t.Name,
		"slug":          t.Slug,
		"status":        t.Status,
		"business_type": t.BusinessType,
	}
	if t.WhatsappNumber != nil {
		out["whatsapp_number"] = *t.WhatsappNumber
	}
	if t.FboDistribID != nil {
		out["fbo_distrib_id"] = *t.FboDistribID
	}
	if t.FlpStoreCode != nil {
		out["flp_store_code"] = *t.FlpStoreCode
	}
	if t.FlpLanguage != nil {
		out["flp_language"] = *t.FlpLanguage
	}
	if t.Country != nil {
		out["country"] = *t.Country
	}
	if t.Metadata != nil {
		out["metadata"] = t.Metadata
	}
	// The currency every amount is shown and entered in, and where it came from (the tenant's
	// choice, its SSO country, or the platform default).
	out["currency"], out["currency_source"] = ResolveCurrency(t.Metadata, t.Country, PlatformCurrency)
	return out
}

// PlatformCurrency is the platform default currency (MF_MONEY_DEFAULT_CURRENCY), set at startup.
var PlatformCurrency = "KES"
