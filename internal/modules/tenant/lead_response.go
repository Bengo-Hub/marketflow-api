package tenant

import (
	"net/http"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/nurturesequence"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/platform/leadresponse"
)

// GetLeadResponse handles GET /api/v1/tenant/profile/lead-response: the tenant's lead response
// settings (automatic follow-up, reminder times, business hours, daily list) beside the platform
// defaults they override, and the tenant's follow-up sequence for the Nurture link. Saved through
// the profile PATCH as metadata.lead_response by the approving roles.
// @Summary Lead response settings
// @Tags Tenant
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /tenant/profile/lead-response [get]
func (h *Handler) GetLeadResponse(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantHeader(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	t, err := h.db.Tenant.Get(ctx, tenantID)
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "not_found", "tenant not found", nil)
		return
	}
	effective, platform := leadresponse.Load(ctx, h.db, t)
	override, _ := t.Metadata[leadresponse.TenantKey].(map[string]any)
	if override == nil {
		override = map[string]any{}
	}
	var sequence any
	seqs, err := h.db.NurtureSequence.Query().
		Where(nurturesequence.TenantIDEQ(tenantID), nurturesequence.ScopeEQ("tenant"), nurturesequence.TriggerEventEQ(leadresponse.TriggerLeadCreated)).
		Order(ent.Asc(nurturesequence.FieldCreatedAt)).All(ctx)
	if err == nil && len(seqs) > 0 {
		pick := seqs[0]
		for _, s := range seqs {
			if leadresponse.IsDefaultCopy(s) {
				pick = s
				break
			}
		}
		sequence = map[string]any{"id": pick.ID, "name": pick.Name, "active": pick.Active, "steps": len(pick.Steps),
			"is_default": leadresponse.IsDefaultCopy(pick)}
	}
	tz := "Africa/Nairobi"
	if t.Timezone != nil && *t.Timezone != "" {
		tz = *t.Timezone
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{
		"platform":  platform.ToView(),
		"effective": effective.ToView(),
		"tenant":    override,
		"timezone":  tz,
		"sequence":  sequence,
		"can_edit":  h.CanApprove == nil || h.CanApprove(ctx, tenantID),
	})
}
