package contacts

import (
	"fmt"
	"net/http"

	"github.com/Bengo-Hub/pagination"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Handler handles HTTP requests for contacts.
type Handler struct {
	service    *Service
	logger     *zap.Logger
	view360Cfg View360Config
}

// NewHandler creates a new contact handler.
func NewHandler(service *Service, logger *zap.Logger, opts ...func(*Handler)) *Handler {
	h := &Handler{
		service: service,
		logger:  logger.Named("contacts.handler"),
	}
	for _, o := range opts {
		o(h)
	}
	return h
}

// WithView360Config sets the cross-service URL config for the 360° view.
func WithView360Config(cfg View360Config) func(*Handler) {
	return func(h *Handler) { h.view360Cfg = cfg }
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
	q := r.URL.Query().Get("q")
	p := pagination.Parse(r)
	input := ListInput{
		TenantID: tenantID,
		Limit:    p.Limit,
		Offset:   p.Offset,
	}
	if contactType != "" {
		input.ContactType = &contactType
	}
	if q != "" {
		input.Query = &q
	}

	results, total, err := h.service.List(r.Context(), input)
	if err != nil {
		h.logger.Error("failed to list contacts", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "list_failed", "failed to list contacts", nil)
		return
	}

	httpapi.JSON(w, http.StatusOK, pagination.NewResponse(results, total, p))
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

// Get360 handles GET /api/v1/contacts/{id}/360
func (h *Handler) Get360(w http.ResponseWriter, r *http.Request) {
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

	result, err := h.service.Get360(r.Context(), tenantID, contactID, h.view360Cfg)
	if err != nil {
		h.logger.Error("failed to get 360 view", zap.Error(err))
		httpapi.Error(w, http.StatusNotFound, "not_found", "contact not found", nil)
		return
	}

	httpapi.JSON(w, http.StatusOK, result)
}

// GetByEmail handles GET /api/v1/internal/contacts/by-email?email=&tenant_id=
// S2S only — requires X-API-Key, no JWT.
func (h *Handler) GetByEmail(w http.ResponseWriter, r *http.Request) {
	email := r.URL.Query().Get("email")
	if email == "" {
		httpapi.Error(w, http.StatusBadRequest, "missing_email", "email query parameter required", nil)
		return
	}

	tenantIDStr := r.URL.Query().Get("tenant_id")
	tenantID, err := uuid.Parse(tenantIDStr)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "missing_tenant", "tenant_id query parameter required", nil)
		return
	}

	c, err := h.service.ByEmail(r.Context(), tenantID, email)
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "not_found", "contact not found", nil)
		return
	}

	// Return minimal profile only — never the full contact to S2S callers
	httpapi.JSON(w, http.StatusOK, map[string]any{
		"id":         c.ID,
		"first_name": c.FirstName,
		"last_name":  c.LastName,
		"email":      c.Email,
		"phone":      c.Phone,
	})
}

// GetByPhone handles GET /api/v1/internal/contacts/by-phone?phone=&tenant_id=
// S2S only — requires X-API-Key, no JWT.
func (h *Handler) GetByPhone(w http.ResponseWriter, r *http.Request) {
	phone := r.URL.Query().Get("phone")
	if phone == "" {
		httpapi.Error(w, http.StatusBadRequest, "missing_phone", "phone query parameter required", nil)
		return
	}

	tenantIDStr := r.URL.Query().Get("tenant_id")
	tenantID, err := uuid.Parse(tenantIDStr)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "missing_tenant", "tenant_id query parameter required", nil)
		return
	}

	c, err := h.service.ByPhone(r.Context(), tenantID, phone)
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "not_found", "contact not found", nil)
		return
	}

	httpapi.JSON(w, http.StatusOK, map[string]any{
		"id":         c.ID,
		"first_name": c.FirstName,
		"last_name":  c.LastName,
		"email":      c.Email,
		"phone":      c.Phone,
	})
}

