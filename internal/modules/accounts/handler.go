package accounts

import (
	"fmt"
	"net/http"

	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Handler handles HTTP requests for accounts.
type Handler struct {
	service *Service
	logger  *zap.Logger
}

// NewHandler creates a new account handler.
func NewHandler(service *Service, logger *zap.Logger) *Handler {
	return &Handler{
		service: service,
		logger:  logger.Named("accounts.handler"),
	}
}

type createAccountRequest struct {
	Name          string         `json:"name"`
	Industry      *string        `json:"industry"`
	Website       *string        `json:"website"`
	Phone         *string        `json:"phone"`
	Address       map[string]any `json:"address"`
	EmployeeCount *int           `json:"employee_count"`
	AnnualRevenue *float64       `json:"annual_revenue"`
	OwnerUserID   *uuid.UUID     `json:"owner_user_id"`
	Metadata      map[string]any `json:"metadata"`
}

type updateAccountRequest struct {
	Name          *string        `json:"name"`
	Industry      *string        `json:"industry"`
	Website       *string        `json:"website"`
	Phone         *string        `json:"phone"`
	Address       map[string]any `json:"address"`
	EmployeeCount *int           `json:"employee_count"`
	AnnualRevenue *float64       `json:"annual_revenue"`
	OwnerUserID   *uuid.UUID     `json:"owner_user_id"`
	Metadata      map[string]any `json:"metadata"`
}

// Create handles POST /api/v1/accounts
// @Summary Create an account (company)
// @Tags Accounts
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param request body createAccountRequest true "Account data"
// @Success 201 {object} map[string]interface{}
// @Security bearerAuth
// @Router /accounts [post]
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	var req createAccountRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}
	if req.Name == "" {
		httpapi.Error(w, http.StatusBadRequest, "missing_name", "name is required", nil)
		return
	}
	result, err := h.service.Create(r.Context(), CreateInput{
		TenantID:      tenantID,
		Name:          req.Name,
		Industry:      req.Industry,
		Website:       req.Website,
		Phone:         req.Phone,
		Address:       req.Address,
		EmployeeCount: req.EmployeeCount,
		AnnualRevenue: req.AnnualRevenue,
		OwnerUserID:   req.OwnerUserID,
		Metadata:      req.Metadata,
	})
	if err != nil {
		h.logger.Error("failed to create account", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "create_failed", "failed to create account", nil)
		return
	}
	httpapi.JSON(w, http.StatusCreated, result)
}

// List handles GET /api/v1/accounts
// @Summary List accounts
// @Tags Accounts
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param industry query string false "Filter by industry"
// @Param limit query int false "Page size"
// @Param offset query int false "Offset"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /accounts [get]
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	in := ListInput{
		TenantID: tenantID,
		Limit:    parseIntQuery(r, "limit", 20),
		Offset:   parseIntQuery(r, "offset", 0),
	}
	if s := r.URL.Query().Get("industry"); s != "" {
		in.Industry = &s
	}
	if s := r.URL.Query().Get("owner_id"); s != "" {
		if id, err := uuid.Parse(s); err == nil {
			in.OwnerID = &id
		}
	}
	results, total, err := h.service.List(r.Context(), in)
	if err != nil {
		h.logger.Error("failed to list accounts", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "list_failed", "failed to list accounts", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"data": results, "total": total})
}

// Get handles GET /api/v1/accounts/{id}
// @Summary Get an account by ID
// @Tags Accounts
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Account UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /accounts/{id} [get]
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid account ID", nil)
		return
	}
	result, err := h.service.Get(r.Context(), tenantID, id)
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "not_found", "account not found", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, result)
}

// Update handles PUT /api/v1/accounts/{id}
// @Summary Update an account
// @Tags Accounts
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Account UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /accounts/{id} [put]
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid account ID", nil)
		return
	}
	var req updateAccountRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}
	result, err := h.service.Update(r.Context(), tenantID, id, UpdateInput{
		Name:          req.Name,
		Industry:      req.Industry,
		Website:       req.Website,
		Phone:         req.Phone,
		Address:       req.Address,
		EmployeeCount: req.EmployeeCount,
		AnnualRevenue: req.AnnualRevenue,
		OwnerUserID:   req.OwnerUserID,
		Metadata:      req.Metadata,
	})
	if err != nil {
		h.logger.Error("failed to update account", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "update_failed", "failed to update account", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, result)
}

// Delete handles DELETE /api/v1/accounts/{id}
// @Summary Delete an account
// @Tags Accounts
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Account UUID"
// @Success 200 {object} map[string]string
// @Security bearerAuth
// @Router /accounts/{id} [delete]
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid account ID", nil)
		return
	}
	if err := h.service.Delete(r.Context(), tenantID, id); err != nil {
		h.logger.Error("failed to delete account", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "delete_failed", "failed to delete account", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

// ListContacts handles GET /api/v1/accounts/{id}/contacts
// @Summary List contacts linked to an account
// @Tags Accounts
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Account UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /accounts/{id}/contacts [get]
func (h *Handler) ListContacts(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid account ID", nil)
		return
	}
	contacts, err := h.service.ListContacts(r.Context(), tenantID, id)
	if err != nil {
		h.logger.Error("failed to list account contacts", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "list_failed", "failed to list contacts", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"data": contacts})
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
