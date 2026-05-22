package tenant

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/modules/rbac"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// MembersHandler serves team member management endpoints for a tenant.
type MembersHandler struct {
	rbacSvc    *rbac.Service
	authAPIURL string
	serviceKey string
	logger     *zap.Logger
}

// NewMembersHandler creates a new MembersHandler.
func NewMembersHandler(rbacSvc *rbac.Service, authAPIURL, serviceKey string, logger *zap.Logger) *MembersHandler {
	return &MembersHandler{
		rbacSvc:    rbacSvc,
		authAPIURL: authAPIURL,
		serviceKey: serviceKey,
		logger:     logger.Named("tenant.members"),
	}
}

// memberResponse is the per-member payload returned by the API.
type memberResponse struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Status    string    `json:"status"`
	Roles     []string  `json:"roles"`
	CreatedAt time.Time `json:"created_at"`
}

// ListMembers handles GET /api/v1/tenant/members.
// @Summary List team members for the current tenant
// @Tags Tenant
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /tenant/members [get]
func (h *MembersHandler) ListMembers(w http.ResponseWriter, r *http.Request) {
	tenantID, err := uuid.Parse(r.Header.Get("X-Tenant-ID"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "X-Tenant-ID required", nil)
		return
	}
	users, err := h.rbacSvc.ListUsers(r.Context(), tenantID)
	if err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "list_failed", "failed to list members", nil)
		return
	}
	members := make([]memberResponse, 0, len(users))
	for _, u := range users {
		roles, _ := h.rbacSvc.GetUserRoles(r.Context(), tenantID, u.ID)
		roleCodes := make([]string, len(roles))
		for i, ro := range roles {
			roleCodes[i] = ro.RoleCode
		}
		members = append(members, memberResponse{
			ID:        u.ID.String(),
			Email:     u.Email,
			Status:    u.Status,
			Roles:     roleCodes,
			CreatedAt: u.CreatedAt,
		})
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"members": members, "total": len(members)})
}

// InviteMember handles POST /api/v1/tenant/members/invite.
// Proxies the invite request to auth-api which owns user identity.
// @Summary Invite a new team member
// @Tags Tenant
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /tenant/members/invite [post]
func (h *MembersHandler) InviteMember(w http.ResponseWriter, r *http.Request) {
	tenantID, err := uuid.Parse(r.Header.Get("X-Tenant-ID"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "X-Tenant-ID required", nil)
		return
	}

	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
		return
	}

	payload, _ := json.Marshal(body)
	url := fmt.Sprintf("%s/api/v1/tenants/%s/users/invite", h.authAPIURL, tenantID)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "invite_failed", "failed to create request", nil)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", h.serviceKey)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		h.logger.Error("auth-api invite call failed", zap.Error(err))
		httpapi.Error(w, http.StatusBadGateway, "invite_failed", "invite service unavailable", nil)
		return
	}
	defer resp.Body.Close()

	var result map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&result)
	httpapi.JSON(w, resp.StatusCode, result)
}

// UpdateMemberRole handles PUT /api/v1/tenant/members/{userId}.
// @Summary Update a team member's role
// @Tags Tenant
// @Accept json
// @Produce json
// @Param userId path string true "Member UUID"
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /tenant/members/{userId} [put]
func (h *MembersHandler) UpdateMemberRole(w http.ResponseWriter, r *http.Request) {
	tenantID, err := uuid.Parse(r.Header.Get("X-Tenant-ID"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "X-Tenant-ID required", nil)
		return
	}
	userID, err := uuid.Parse(chi.URLParam(r, "userId"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_user_id", "invalid user ID", nil)
		return
	}

	// Extract the requesting user's ID from JWT claims (set by auth middleware).
	var callerID uuid.UUID
	if sub := r.Header.Get("X-User-ID"); sub != "" {
		callerID, _ = uuid.Parse(sub)
	}

	var req struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Role == "" {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "role is required", nil)
		return
	}

	if err := h.rbacSvc.UpdateUserRole(r.Context(), tenantID, userID, req.Role, callerID); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "update_failed", err.Error(), nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

// RemoveMember handles DELETE /api/v1/tenant/members/{userId}.
// @Summary Remove a team member from the tenant
// @Tags Tenant
// @Produce json
// @Param userId path string true "Member UUID"
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /tenant/members/{userId} [delete]
func (h *MembersHandler) RemoveMember(w http.ResponseWriter, r *http.Request) {
	tenantID, err := uuid.Parse(r.Header.Get("X-Tenant-ID"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "X-Tenant-ID required", nil)
		return
	}
	userID, err := uuid.Parse(chi.URLParam(r, "userId"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_user_id", "invalid user ID", nil)
		return
	}

	if err := h.rbacSvc.DeactivateUser(r.Context(), tenantID, userID); err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "remove_failed", "failed to remove member", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"ok": true})
}
