package meetings

import (
	"fmt"
	"net/http"
	"time"

	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Handler handles HTTP requests for scheduled meetings.
type Handler struct {
	service *Service
	logger  *zap.Logger
}

// NewHandler creates a new scheduled meeting handler.
func NewHandler(service *Service, logger *zap.Logger) *Handler {
	return &Handler{
		service: service,
		logger:  logger.Named("meetings.handler"),
	}
}

type createMeetingRequest struct {
	LeadID          *uuid.UUID     `json:"lead_id"`
	ContactID       *uuid.UUID     `json:"contact_id"`
	CalBookingID    *string        `json:"cal_booking_id"`
	MeetingURL      *string        `json:"meeting_url"`
	ScheduledAt     time.Time      `json:"scheduled_at"`
	DurationMinutes *int           `json:"duration_minutes"`
	MeetingType     *string        `json:"meeting_type"`
	Notes           *string        `json:"notes"`
	Metadata        map[string]any `json:"metadata"`
	AttendeeEmail   *string        `json:"attendee_email"`
	AttendeeName    *string        `json:"attendee_name"`
	OrganizerEmail  *string        `json:"organizer_email"`
}

type updateMeetingRequest struct {
	LeadID          *uuid.UUID     `json:"lead_id"`
	ContactID       *uuid.UUID     `json:"contact_id"`
	CalBookingID    *string        `json:"cal_booking_id"`
	MeetingURL      *string        `json:"meeting_url"`
	ScheduledAt     *time.Time     `json:"scheduled_at"`
	DurationMinutes *int           `json:"duration_minutes"`
	MeetingType     *string        `json:"meeting_type"`
	Status          *string        `json:"status"`
	Notes           *string        `json:"notes"`
	Metadata        map[string]any `json:"metadata"`
}

// Create handles POST /api/v1/meetings
// @Summary Create a new scheduled meeting
// @Description Creates a virtual meeting booking (Cal.com or manual)
// @Tags Meetings
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param request body createMeetingRequest true "Meeting data"
// @Success 201 {object} map[string]interface{}
// @Failure 400 {object} httpapi.ErrorResponse
// @Failure 500 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /meetings [post]
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	var req createMeetingRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}

	if req.ScheduledAt.IsZero() {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "scheduled_at is required", nil)
		return
	}

	result, err := h.service.Create(r.Context(), CreateInput{
		TenantID:        tenantID,
		LeadID:          req.LeadID,
		ContactID:       req.ContactID,
		CalBookingID:    req.CalBookingID,
		MeetingURL:      req.MeetingURL,
		ScheduledAt:     req.ScheduledAt,
		DurationMinutes: req.DurationMinutes,
		MeetingType:     req.MeetingType,
		Notes:           req.Notes,
		Metadata:        req.Metadata,
		AttendeeEmail:   req.AttendeeEmail,
		AttendeeName:    req.AttendeeName,
		OrganizerEmail:  req.OrganizerEmail,
	})
	if err != nil {
		h.logger.Error("failed to create meeting", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "create_failed", "failed to create meeting", nil)
		return
	}

	httpapi.JSON(w, http.StatusCreated, result)
}

// List handles GET /api/v1/meetings
// @Summary List scheduled meetings
// @Description Returns paginated meetings with optional filters
// @Tags Meetings
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param status query string false "Filter by status: scheduled, completed, no_show, cancelled, rescheduled"
// @Param meeting_type query string false "Filter by type: product_demo, business_opportunity, follow_up, onboarding"
// @Param date_from query string false "Filter meetings from date (RFC3339)"
// @Param date_to query string false "Filter meetings to date (RFC3339)"
// @Param limit query int false "Page size (default 20, max 100)"
// @Param offset query int false "Offset for pagination"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /meetings [get]
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	status := r.URL.Query().Get("status")
	meetingType := r.URL.Query().Get("meeting_type")
	dateFromStr := r.URL.Query().Get("date_from")
	dateToStr := r.URL.Query().Get("date_to")

	input := ListInput{
		TenantID: tenantID,
		Limit:    parseIntQuery(r, "limit", 20),
		Offset:   parseIntQuery(r, "offset", 0),
	}
	if status != "" {
		input.Status = &status
	}
	if meetingType != "" {
		input.MeetingType = &meetingType
	}
	if dateFromStr != "" {
		if t, err := time.Parse(time.RFC3339, dateFromStr); err == nil {
			input.DateFrom = &t
		}
	}
	if dateToStr != "" {
		if t, err := time.Parse(time.RFC3339, dateToStr); err == nil {
			input.DateTo = &t
		}
	}

	results, total, err := h.service.List(r.Context(), input)
	if err != nil {
		h.logger.Error("failed to list meetings", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "list_failed", "failed to list meetings", nil)
		return
	}

	httpapi.JSON(w, http.StatusOK, map[string]any{
		"data":  results,
		"total": total,
	})
}

// Get handles GET /api/v1/meetings/{id}
// @Summary Get a scheduled meeting by ID
// @Tags Meetings
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Meeting UUID"
// @Success 200 {object} map[string]interface{}
// @Failure 404 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /meetings/{id} [get]
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	meetingID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid meeting ID", nil)
		return
	}

	result, err := h.service.Get(r.Context(), tenantID, meetingID)
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "not_found", "meeting not found", nil)
		return
	}

	httpapi.JSON(w, http.StatusOK, result)
}

// Update handles PUT /api/v1/meetings/{id}
// @Summary Update a scheduled meeting
// @Tags Meetings
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Meeting UUID"
// @Param request body updateMeetingRequest true "Fields to update"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /meetings/{id} [put]
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	meetingID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid meeting ID", nil)
		return
	}

	var req updateMeetingRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}

	result, err := h.service.Update(r.Context(), tenantID, meetingID, UpdateInput{
		LeadID:          req.LeadID,
		ContactID:       req.ContactID,
		CalBookingID:    req.CalBookingID,
		MeetingURL:      req.MeetingURL,
		ScheduledAt:     req.ScheduledAt,
		DurationMinutes: req.DurationMinutes,
		MeetingType:     req.MeetingType,
		Status:          req.Status,
		Notes:           req.Notes,
		Metadata:        req.Metadata,
	})
	if err != nil {
		h.logger.Error("failed to update meeting", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "update_failed", "failed to update meeting", nil)
		return
	}

	httpapi.JSON(w, http.StatusOK, result)
}

// extractTenantID extracts tenant ID from X-Tenant-ID header.
func extractTenantID(r *http.Request) (uuid.UUID, error) {
	header := r.Header.Get("X-Tenant-ID")
	if header == "" {
		return uuid.UUID{}, fmt.Errorf("X-Tenant-ID header is required")
	}
	return uuid.Parse(header)
}

// parseIntQuery parses an integer query parameter with a default value.
func parseIntQuery(r *http.Request, key string, defaultVal int) int {
	val := r.URL.Query().Get(key)
	if val == "" {
		return defaultVal
	}
	n := 0
	for _, c := range val {
		if c < '0' || c > '9' {
			return defaultVal
		}
		n = n*10 + int(c-'0')
	}
	return n
}
