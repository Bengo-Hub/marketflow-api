package webhooks

import (
	"net/http"
	"time"

	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/modules/leads"
	"github.com/bengobox/marketflow-api/internal/modules/meetings"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// CalcomHandler handles Cal.com booking webhooks.
type CalcomHandler struct {
	meetingService *meetings.Service
	leadService    *leads.Service
	logger         *zap.Logger
}

// NewCalcomHandler creates a new Cal.com webhook handler.
func NewCalcomHandler(meetingService *meetings.Service, leadService *leads.Service, logger *zap.Logger) *CalcomHandler {
	return &CalcomHandler{
		meetingService: meetingService,
		leadService:    leadService,
		logger:         logger.Named("webhooks.calcom"),
	}
}

type calcomBookingPayload struct {
	TriggerEvent string           `json:"triggerEvent"`
	Payload      calcomBookingData `json:"payload"`
}

type calcomBookingData struct {
	TenantID     string        `json:"tenant_id"`
	BookingID    string        `json:"uid"`
	Title        string        `json:"title"`
	StartTime    string        `json:"startTime"`
	EndTime      string        `json:"endTime"`
	MeetingURL   string        `json:"meetingUrl"`
	MeetingType  string        `json:"meeting_type"`
	LeadID       string        `json:"lead_id"`
	ContactID    string        `json:"contact_id"`
	Notes        string        `json:"description"`
	Metadata     map[string]any `json:"metadata"`
	Attendees    []calcomAttendee `json:"attendees"`
}

type calcomAttendee struct {
	Email    string `json:"email"`
	Name     string `json:"name"`
	TimeZone string `json:"timeZone"`
}

// HandleBooking handles POST /api/v1/webhooks/calcom/booking
// @Summary Handle Cal.com booking webhook
// @Description Receives booking events from Cal.com (booking_created)
// @Tags Webhooks
// @Accept json
// @Produce json
// @Param request body calcomBookingPayload true "Cal.com booking webhook payload"
// @Success 200 {object} map[string]string
// @Failure 400 {object} httpapi.ErrorResponse
// @Router /webhooks/calcom/booking [post]
func (h *CalcomHandler) HandleBooking(w http.ResponseWriter, r *http.Request) {
	var req calcomBookingPayload
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		h.logger.Warn("invalid calcom webhook payload", zap.Error(err))
		httpapi.JSON(w, http.StatusOK, map[string]any{"status": "received"})
		return
	}

	// Only process booking_created events
	if req.TriggerEvent != "BOOKING_CREATED" && req.TriggerEvent != "booking_created" {
		h.logger.Info("ignoring calcom event", zap.String("event", req.TriggerEvent))
		httpapi.JSON(w, http.StatusOK, map[string]any{"status": "ignored"})
		return
	}

	tenantID, err := uuid.Parse(req.Payload.TenantID)
	if err != nil {
		h.logger.Warn("invalid tenant_id in calcom webhook", zap.String("tenant_id", req.Payload.TenantID))
		httpapi.JSON(w, http.StatusOK, map[string]any{"status": "received"})
		return
	}

	// Parse scheduled time
	scheduledAt, err := time.Parse(time.RFC3339, req.Payload.StartTime)
	if err != nil {
		h.logger.Warn("invalid startTime in calcom webhook", zap.String("start_time", req.Payload.StartTime))
		httpapi.JSON(w, http.StatusOK, map[string]any{"status": "received"})
		return
	}

	// Calculate duration
	durationMinutes := 30
	if endTime, err := time.Parse(time.RFC3339, req.Payload.EndTime); err == nil {
		durationMinutes = int(endTime.Sub(scheduledAt).Minutes())
		if durationMinutes <= 0 {
			durationMinutes = 30
		}
	}

	input := meetings.CreateInput{
		TenantID:        tenantID,
		ScheduledAt:     scheduledAt,
		DurationMinutes: &durationMinutes,
	}

	if req.Payload.BookingID != "" {
		input.CalBookingID = &req.Payload.BookingID
	}
	if req.Payload.MeetingURL != "" {
		input.MeetingURL = &req.Payload.MeetingURL
	}
	if req.Payload.MeetingType != "" {
		input.MeetingType = &req.Payload.MeetingType
	}
	if req.Payload.Notes != "" {
		input.Notes = &req.Payload.Notes
	}
	if req.Payload.LeadID != "" {
		if lid, err := uuid.Parse(req.Payload.LeadID); err == nil {
			input.LeadID = &lid
		}
	}
	if req.Payload.ContactID != "" {
		if cid, err := uuid.Parse(req.Payload.ContactID); err == nil {
			input.ContactID = &cid
		}
	}
	input.Metadata = map[string]any{
		"webhook_source":   "calcom",
		"webhook_received": time.Now().UTC().Format(time.RFC3339),
		"cal_booking_uid":  req.Payload.BookingID,
		"title":            req.Payload.Title,
	}

	if _, err := h.meetingService.Create(r.Context(), input); err != nil {
		h.logger.Error("failed to create meeting from calcom webhook", zap.Error(err))
		httpapi.JSON(w, http.StatusOK, map[string]any{"status": "error", "message": "failed to process booking"})
		return
	}

	// Update lead status to qualified if lead_id present
	if input.LeadID != nil {
		qualified := "qualified"
		if _, err := h.leadService.Update(r.Context(), tenantID, *input.LeadID, leads.UpdateInput{
			Status: &qualified,
		}); err != nil {
			h.logger.Warn("failed to update lead status from calcom webhook",
				zap.String("lead_id", input.LeadID.String()),
				zap.Error(err),
			)
		}
	}

	h.logger.Info("meeting created from calcom webhook",
		zap.String("tenant_id", tenantID.String()),
		zap.String("booking_id", req.Payload.BookingID),
	)

	httpapi.JSON(w, http.StatusOK, map[string]any{"status": "ok"})
}
