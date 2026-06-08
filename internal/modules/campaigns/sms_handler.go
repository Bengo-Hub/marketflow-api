package campaigns

import (
	"net/http"
	"strings"

	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

type sendSMSRequest struct {
	SegmentID string `json:"segment_id"`
	Message   string `json:"message"`
}

// SendSMS handles POST /api/v1/{tenantID}/campaigns/{campaignID}/send-sms
// @Summary Send a bulk SMS for a campaign
// @Description Resolves the target CRM contacts and queues one SMS event per contact via the outbox; notifications-api delivers them.
// @Tags Campaigns
// @Accept json
// @Produce json
// @Param tenantID path string true "Tenant UUID"
// @Param campaignID path string true "Campaign UUID"
// @Param request body sendSMSRequest true "Bulk SMS request"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} httpapi.ErrorResponse
// @Failure 404 {object} httpapi.ErrorResponse
// @Failure 500 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /{tenantID}/campaigns/{campaignID}/send-sms [post]
func (h *Handler) SendSMS(w http.ResponseWriter, r *http.Request) {
	tenantID, err := uuid.Parse(chi.URLParam(r, "tenantID"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "invalid tenant ID", nil)
		return
	}

	campaignID, err := uuid.Parse(chi.URLParam(r, "campaignID"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid campaign ID", nil)
		return
	}

	var req sendSMSRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}

	req.Message = strings.TrimSpace(req.Message)
	if req.Message == "" {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "message is required", nil)
		return
	}

	in := SendSMSInput{
		TenantID:   tenantID,
		CampaignID: campaignID,
		Message:    req.Message,
	}
	if sid := strings.TrimSpace(req.SegmentID); sid != "" {
		segmentID, err := uuid.Parse(sid)
		if err != nil {
			httpapi.Error(w, http.StatusBadRequest, "invalid_segment", "invalid segment_id", nil)
			return
		}
		in.SegmentID = &segmentID
	}

	queued, err := h.service.SendSMS(r.Context(), in)
	if err != nil {
		h.logger.Error("failed to queue bulk sms", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "send_sms_failed", "failed to queue bulk SMS", nil)
		return
	}

	httpapi.JSON(w, http.StatusOK, map[string]any{
		"queued":      queued,
		"campaign_id": campaignID.String(),
	})
}
