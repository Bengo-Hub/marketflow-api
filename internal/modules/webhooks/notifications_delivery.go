package webhooks

import (
	"errors"
	"net/http"

	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/modules/nurturedelivery"
	"go.uber.org/zap"
)

// NotificationsDeliveryRequest is the payload notifications-api sends when a downstream channel
// emits a delivery / open / click / bounce / reply event. notifications-api now publishes the
// same payload on notifications.engagement.recorded, which the worker consumes; this endpoint
// stays for any sender that posts directly.
type NotificationsDeliveryRequest = nurturedelivery.Input

// NotificationsDelivery handles POST /api/v1/webhooks/notifications/delivery.
//
// Authenticated via X-API-Key (platform service key): notifications-api is an internal peer,
// not user-facing. Silently no-ops when the envelope has no enrollment_id (ad-hoc transactional
// mail doesn't feed the nurture funnel).
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
	v, err := nurturedelivery.Validate(req)
	if err != nil {
		var ve *nurturedelivery.ValidationError
		if errors.As(err, &ve) {
			httpapi.Error(w, http.StatusBadRequest, ve.Code, ve.Message, nil)
			return
		}
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", err.Error(), nil)
		return
	}
	if _, err := nurturedelivery.Record(r.Context(), h.entClient, v, ""); err != nil {
		h.logger.Warn("delivery event save failed",
			zap.String("event", v.EventType), zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "save_failed", err.Error(), nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
