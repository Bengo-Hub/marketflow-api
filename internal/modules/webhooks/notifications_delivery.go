package webhooks

import (
	"net/http"
	"strings"

	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// NotificationsDeliveryRequest is the payload notifications-api sends when a
// downstream channel emits a delivery / open / click / bounce / reply event.
// Matches the envelope defined in notifications-api's webhook publisher — see
// shared-docs/notifications-webhook-contract.md.
type NotificationsDeliveryRequest struct {
	TenantID          string         `json:"tenant_id"`
	EnrollmentID      string         `json:"enrollment_id,omitempty"`
	StepIndex         int            `json:"step_index,omitempty"`
	Channel           string         `json:"channel"`
	EventType         string         `json:"event_type"`
	ProviderMessageID string         `json:"provider_message_id,omitempty"`
	LinkURL           string         `json:"link_url,omitempty"`
	Metadata          map[string]any `json:"metadata,omitempty"`
}

var validDeliveryEvents = map[string]struct{}{
	"sent":         {},
	"delivered":    {},
	"opened":       {},
	"clicked":      {},
	"bounced":      {},
	"replied":      {},
	"unsubscribed": {},
	"failed":       {},
}

// NotificationsDelivery handles POST /api/v1/webhooks/notifications/delivery.
//
// Authenticated via X-API-Key (platform service key) — notifications-api is
// an internal peer, not user-facing. Silently no-ops when the envelope has
// no enrollment_id (ad-hoc transactional mail doesn't feed the nurture funnel).
//
// @Summary Receive delivery telemetry from notifications-api
// @Tags Webhooks
// @Accept json
// @Produce json
// @Param event body NotificationsDeliveryRequest true "Delivery event"
// @Success 204
// @Failure 400 {object} httpapi.ErrorResponse
// @Router /webhooks/notifications/delivery [post]
func (h *PublicHandler) NotificationsDelivery(w http.ResponseWriter, r *http.Request) {
	var req NotificationsDeliveryRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
		return
	}
	req.EventType = strings.ToLower(strings.TrimSpace(req.EventType))
	req.Channel = strings.ToLower(strings.TrimSpace(req.Channel))
	if req.TenantID == "" || req.EventType == "" {
		httpapi.Error(w, http.StatusBadRequest, "missing_fields", "tenant_id and event_type are required", nil)
		return
	}
	if _, ok := validDeliveryEvents[req.EventType]; !ok {
		httpapi.Error(w, http.StatusBadRequest, "unknown_event_type", "unknown event_type", nil)
		return
	}
	tenantID, err := uuid.Parse(req.TenantID)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant_id", err.Error(), nil)
		return
	}
	if req.EnrollmentID == "" {
		// Not every outbound message is nurture-related. Noop for
		// transactional mail but return 204 so the sender sees success.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	enrollmentID, err := uuid.Parse(req.EnrollmentID)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_enrollment_id", err.Error(), nil)
		return
	}

	builder := h.entClient.NurtureDeliveryEvent.Create().
		SetTenantID(tenantID).
		SetEnrollmentID(enrollmentID).
		SetStepIndex(req.StepIndex).
		SetChannel(firstNonEmpty(req.Channel, "email")).
		SetEventType(req.EventType)
	if req.ProviderMessageID != "" {
		builder = builder.SetProviderMessageID(req.ProviderMessageID)
	}
	if req.LinkURL != "" {
		builder = builder.SetLinkURL(req.LinkURL)
	}
	if req.Metadata != nil {
		builder = builder.SetMetadata(req.Metadata)
	}
	if _, err := builder.Save(r.Context()); err != nil {
		h.logger.Warn("delivery event save failed",
			zap.String("event", req.EventType), zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "save_failed", err.Error(), nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
