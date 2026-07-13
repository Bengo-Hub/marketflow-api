package webhooks

import (
	"errors"
	"net/http"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent/scheduledmeeting"
	"github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/integrations/gcal"
	"github.com/bengobox/marketflow-api/internal/modules/leads"
	"github.com/bengobox/marketflow-api/internal/modules/meetings"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const defaultTenantTimezone = "Africa/Nairobi"

func (h *PublicHandler) resolveTenantForBooking(w http.ResponseWriter, r *http.Request) (uuid.UUID, string, bool) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		httpapi.Error(w, http.StatusBadRequest, "invalid_slug", "slug is required", nil)
		return uuid.Nil, "", false
	}
	t, err := h.entClient.Tenant.Query().
		Where(tenant.SlugEQ(slug), tenant.StatusEQ("active")).
		Only(r.Context())
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "tenant_not_found", "no tenant for slug", nil)
		return uuid.Nil, "", false
	}
	tz := defaultTenantTimezone
	if t.Timezone != nil && *t.Timezone != "" {
		tz = *t.Timezone
	}
	return t.ID, tz, true
}

// GetAvailability handles GET /api/v1/public/availability/{slug}
// @Summary Public booking availability
// @Description Returns open time slots for the tenant's linked Google Calendar on the given date. `connected: false` means the tenant hasn't linked a calendar yet.
// @Tags Public
// @Produce json
// @Param slug path string true "Tenant URL slug"
// @Param date query string true "Date to check, YYYY-MM-DD"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} httpapi.ErrorResponse
// @Failure 404 {object} httpapi.ErrorResponse
// @Router /public/availability/{slug} [get]
func (h *PublicHandler) GetAvailability(w http.ResponseWriter, r *http.Request) {
	tenantID, tz, ok := h.resolveTenantForBooking(w, r)
	if !ok {
		return
	}

	dateStr := r.URL.Query().Get("date")
	if dateStr == "" {
		httpapi.Error(w, http.StatusBadRequest, "missing_date", "date query param (YYYY-MM-DD) is required", nil)
		return
	}
	date, err := time.Parse("2006-01-02", dateStr)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_date", "date must be YYYY-MM-DD", nil)
		return
	}

	slots, err := h.availabilitySvc.ComputeSlots(r.Context(), tenantID.String(), date, tz)
	if err != nil {
		if errors.Is(err, gcal.ErrNotConnected) {
			httpapi.JSON(w, http.StatusOK, map[string]any{"connected": false, "slots": []any{}})
			return
		}
		h.logger.Error("compute availability failed", zap.String("tenant_id", tenantID.String()), zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "availability_failed", "could not compute availability", nil)
		return
	}

	out := make([]map[string]any, 0, len(slots))
	for _, s := range slots {
		out = append(out, map[string]any{
			"start": s.Start.Format(time.RFC3339),
			"end":   s.End.Format(time.RFC3339),
		})
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{
		"connected": true,
		"timezone":  tz,
		"date":      dateStr,
		"slots":     out,
	})
}

type publicBookingInput struct {
	SlotStart   string `json:"slot_start"`
	Name        string `json:"name"`
	Email       string `json:"email"`
	Phone       string `json:"phone"`
	LeadID      string `json:"lead_id"`
	Notes       string `json:"notes"`
	MeetingType string `json:"meeting_type"`
}

// CreateBooking handles POST /api/v1/public/book/{slug}
// @Summary Public booking creation
// @Description Books a meeting slot on the tenant's linked Google Calendar. Re-validates the slot is still open immediately before creating it.
// @Tags Public
// @Accept json
// @Produce json
// @Param slug path string true "Tenant URL slug"
// @Param request body publicBookingInput true "Booking data"
// @Success 201 {object} map[string]interface{}
// @Failure 400 {object} httpapi.ErrorResponse
// @Failure 404 {object} httpapi.ErrorResponse
// @Failure 409 {object} httpapi.ErrorResponse
// @Router /public/book/{slug} [post]
func (h *PublicHandler) CreateBooking(w http.ResponseWriter, r *http.Request) {
	tenantID, tz, ok := h.resolveTenantForBooking(w, r)
	if !ok {
		return
	}

	var in publicBookingInput
	if err := httpapi.DecodeJSON(r, &in); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid JSON body", nil)
		return
	}
	if in.Name == "" || in.Email == "" {
		httpapi.Error(w, http.StatusBadRequest, "missing_contact", "name and email are required", nil)
		return
	}
	slotStart, err := time.Parse(time.RFC3339, in.SlotStart)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_slot", "slot_start must be RFC3339", nil)
		return
	}
	if slotStart.Before(time.Now()) {
		httpapi.Error(w, http.StatusBadRequest, "slot_in_past", "slot_start must be in the future", nil)
		return
	}

	// Re-validate the slot is still open right before booking it: re-query
	// FreeBusy (catches concurrent bookings on the same calendar) and check
	// for a conflicting local ScheduledMeeting row (catches a prior booking
	// whose gcal push silently failed, per Service.Create's non-fatal sync).
	slots, err := h.availabilitySvc.ComputeSlots(r.Context(), tenantID.String(), slotStart, tz)
	if err != nil {
		if errors.Is(err, gcal.ErrNotConnected) {
			httpapi.Error(w, http.StatusConflict, "not_connected", "this tenant hasn't connected a calendar", nil)
			return
		}
		h.logger.Error("re-validate availability failed", zap.String("tenant_id", tenantID.String()), zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "availability_failed", "could not verify availability", nil)
		return
	}
	if !slotStillOpen(slotStart, slots) {
		httpapi.Error(w, http.StatusConflict, "slot_unavailable", "that time is no longer available", nil)
		return
	}
	conflict, err := h.entClient.ScheduledMeeting.Query().
		Where(
			scheduledmeeting.TenantID(tenantID),
			scheduledmeeting.ScheduledAt(slotStart),
			scheduledmeeting.StatusEQ("scheduled"),
		).
		Exist(r.Context())
	if err != nil {
		h.logger.Error("conflict check failed", zap.String("tenant_id", tenantID.String()), zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "availability_failed", "could not verify availability", nil)
		return
	}
	if conflict {
		httpapi.Error(w, http.StatusConflict, "slot_unavailable", "that time is no longer available", nil)
		return
	}

	var leadID *uuid.UUID
	if in.LeadID != "" {
		id, err := uuid.Parse(in.LeadID)
		if err != nil {
			httpapi.Error(w, http.StatusBadRequest, "invalid_lead_id", "lead_id must be a UUID", nil)
			return
		}
		if _, err := h.leadService.Get(r.Context(), tenantID, id); err != nil {
			httpapi.Error(w, http.StatusBadRequest, "lead_not_found", "lead_id does not belong to this tenant", nil)
			return
		}
		leadID = &id
	} else {
		email, name := in.Email, in.Name
		lead, err := h.leadService.Create(r.Context(), leads.CreateInput{
			TenantID:  tenantID,
			Source:    "booking",
			LeadType:  "customer",
			Email:     &email,
			FirstName: &name,
			Metadata: map[string]any{
				"captured_via": "booking_widget",
				"captured_at":  time.Now().UTC().Format(time.RFC3339),
			},
		})
		if err != nil {
			h.logger.Error("lead create for booking failed", zap.String("tenant_id", tenantID.String()), zap.Error(err))
			httpapi.Error(w, http.StatusInternalServerError, "create_failed", "could not create lead", nil)
			return
		}
		leadID = &lead.ID
	}

	meetingType := in.MeetingType
	if meetingType == "" {
		meetingType = "product_demo"
	}
	createInput := meetings.CreateInput{
		TenantID:      tenantID,
		LeadID:        leadID,
		ScheduledAt:   slotStart,
		MeetingType:   &meetingType,
		AttendeeEmail: &in.Email,
		AttendeeName:  &in.Name,
	}
	if in.Notes != "" {
		createInput.Notes = &in.Notes
	}
	meeting, err := h.meetingService.Create(r.Context(), createInput)
	if err != nil {
		h.logger.Error("meeting create for booking failed", zap.String("tenant_id", tenantID.String()), zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "create_failed", "could not create booking", nil)
		return
	}

	if leadID != nil {
		qualified := "qualified"
		if _, err := h.leadService.Update(r.Context(), tenantID, *leadID, leads.UpdateInput{Status: &qualified}); err != nil {
			h.logger.Warn("failed to qualify lead from booking",
				zap.String("lead_id", leadID.String()), zap.Error(err))
		}
	}

	resp := map[string]any{
		"meeting_id":   meeting.ID,
		"scheduled_at": meeting.ScheduledAt,
	}
	if meeting.MeetingURL != nil {
		resp["meeting_url"] = *meeting.MeetingURL
	}
	if meeting.GoogleEventLink != nil {
		resp["google_event_link"] = *meeting.GoogleEventLink
	}
	if leadID != nil {
		resp["lead_id"] = *leadID
	}
	httpapi.JSON(w, http.StatusCreated, resp)
}

func slotStillOpen(start time.Time, slots []gcal.Slot) bool {
	for _, s := range slots {
		if s.Start.Equal(start) {
			return true
		}
	}
	return false
}
