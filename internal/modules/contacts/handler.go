package contacts

import (
	"fmt"
	"net/http"

	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Handler handles HTTP requests for contacts.
type Handler struct {
	service *Service
	logger  *zap.Logger
}

// NewHandler creates a new contact handler.
func NewHandler(service *Service, logger *zap.Logger) *Handler {
	return &Handler{
		service: service,
		logger:  logger.Named("contacts.handler"),
	}
}

type createContactRequest struct {
	Email            *string        `json:"email"`
	Phone            *string        `json:"phone"`
	FirstName        *string        `json:"first_name"`
	LastName         *string        `json:"last_name"`
	ContactType      string         `json:"contact_type"`
	LeadID           *uuid.UUID     `json:"lead_id"`
	FLPCustomerID    *string        `json:"flp_customer_id"`
	ProductInterests []string       `json:"product_interests"`
	Metadata         map[string]any `json:"metadata"`
}

type updateContactRequest struct {
	Email            *string        `json:"email"`
	Phone            *string        `json:"phone"`
	FirstName        *string        `json:"first_name"`
	LastName         *string        `json:"last_name"`
	ContactType      *string        `json:"contact_type"`
	FLPCustomerID    *string        `json:"flp_customer_id"`
	TotalCC          *float64       `json:"total_cc"`
	TotalOrders      *int           `json:"total_orders"`
	ProductInterests []string       `json:"product_interests"`
	SubscribedEmail  *bool          `json:"subscribed_email"`
	SubscribedSMS    *bool          `json:"subscribed_sms"`
	Metadata         map[string]any `json:"metadata"`
}

// Create handles POST /api/v1/contacts
// @Summary Create a contact
// @Description Creates a contact with auto-deduplication by email/phone
// @Tags Contacts
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param request body createContactRequest true "Contact data"
// @Success 201 {object} map[string]interface{}
// @Security bearerAuth
// @Router /contacts [post]
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	var req createContactRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}

	if req.ContactType == "" {
		req.ContactType = "customer"
	}

	result, err := h.service.Create(r.Context(), CreateInput{
		TenantID:         tenantID,
		Email:            req.Email,
		Phone:            req.Phone,
		FirstName:        req.FirstName,
		LastName:         req.LastName,
		ContactType:      req.ContactType,
		LeadID:           req.LeadID,
		FLPCustomerID:    req.FLPCustomerID,
		ProductInterests: req.ProductInterests,
		Metadata:         req.Metadata,
	})
	if err != nil {
		h.logger.Error("failed to create contact", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "create_failed", "failed to create contact", nil)
		return
	}

	httpapi.JSON(w, http.StatusCreated, result)
}

// List handles GET /api/v1/contacts
// @Summary List contacts
// @Tags Contacts
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param contact_type query string false "Filter: customer, prospect, recruit, downline"
// @Param limit query int false "Page size"
// @Param offset query int false "Offset"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /contacts [get]
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	contactType := r.URL.Query().Get("contact_type")
	input := ListInput{
		TenantID: tenantID,
		Limit:    parseIntQuery(r, "limit", 20),
		Offset:   parseIntQuery(r, "offset", 0),
	}
	if contactType != "" {
		input.ContactType = &contactType
	}

	results, total, err := h.service.List(r.Context(), input)
	if err != nil {
		h.logger.Error("failed to list contacts", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "list_failed", "failed to list contacts", nil)
		return
	}

	httpapi.JSON(w, http.StatusOK, map[string]any{
		"data":  results,
		"total": total,
	})
}

// Get handles GET /api/v1/contacts/{id}
// @Summary Get a contact by ID
// @Tags Contacts
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Contact UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /contacts/{id} [get]
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	contactID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid contact ID", nil)
		return
	}

	result, err := h.service.Get(r.Context(), tenantID, contactID)
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "not_found", "contact not found", nil)
		return
	}

	httpapi.JSON(w, http.StatusOK, result)
}

// Update handles PUT /api/v1/contacts/{id}
// @Summary Update a contact
// @Tags Contacts
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Contact UUID"
// @Param request body updateContactRequest true "Fields to update"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /contacts/{id} [put]
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	contactID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid contact ID", nil)
		return
	}

	var req updateContactRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}

	result, err := h.service.Update(r.Context(), tenantID, contactID, UpdateInput{
		Email:            req.Email,
		Phone:            req.Phone,
		FirstName:        req.FirstName,
		LastName:         req.LastName,
		ContactType:      req.ContactType,
		FLPCustomerID:    req.FLPCustomerID,
		TotalCC:          req.TotalCC,
		TotalOrders:      req.TotalOrders,
		ProductInterests: req.ProductInterests,
		SubscribedEmail:  req.SubscribedEmail,
		SubscribedSMS:    req.SubscribedSMS,
		Metadata:         req.Metadata,
	})
	if err != nil {
		h.logger.Error("failed to update contact", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "update_failed", "failed to update contact", nil)
		return
	}

	httpapi.JSON(w, http.StatusOK, result)
}

// Delete handles DELETE /api/v1/contacts/{id}
// @Summary Delete a contact
// @Tags Contacts
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Contact UUID"
// @Success 200 {object} map[string]string
// @Security bearerAuth
// @Router /contacts/{id} [delete]
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	contactID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid contact ID", nil)
		return
	}

	if err := h.service.Delete(r.Context(), tenantID, contactID); err != nil {
		h.logger.Error("failed to delete contact", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "delete_failed", "failed to delete contact", nil)
		return
	}

	httpapi.JSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

func extractTenantID(r *http.Request) (uuid.UUID, error) {
	header := r.Header.Get("X-Tenant-ID")
	if header == "" {
		return uuid.UUID{}, fmt.Errorf("X-Tenant-ID header is required")
	}
	return uuid.Parse(header)
}

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
