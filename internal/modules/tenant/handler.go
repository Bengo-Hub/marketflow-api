package tenant

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/modules/audit"
	"github.com/bengobox/marketflow-api/internal/modules/tenant/metastore"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Handler serves tenant profile settings (FBO branding, phone, bio, etc.).
type Handler struct {
	db     *ent.Client
	logger *zap.Logger
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
	if req.BusinessType != nil {
		bt := strings.TrimSpace(*req.BusinessType)
		if bt != BusinessTypeFLP && bt != BusinessTypeGeneric {
			httpapi.Error(w, http.StatusBadRequest, "invalid_business_type", "business_type must be flp or generic", nil)
			return
		}
		upd = upd.SetBusinessType(bt)
	}

	if _, err := upd.Save(r.Context()); err != nil {
		h.logger.Error("save profile", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "save_failed", "could not save the profile", nil)
		return
	}
	if patch := ProfileMetadataPatch(req.Metadata); len(patch) > 0 {
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
	updated, err := h.db.Tenant.Get(r.Context(), tenantID)
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
	return out
}
