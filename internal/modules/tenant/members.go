package tenant

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/mail"
	"slices"
	"strings"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/modules/audit"
	"github.com/bengobox/marketflow-api/internal/modules/rbac"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// MembersHandler serves team member management endpoints for a tenant.
// Reading the team is open to any member; inviting, changing roles and removing
// need the tenant's marketflow_admin role (or a platform owner or service key).
type MembersHandler struct {
	rbacSvc    *rbac.Service
	db         *ent.Client
	authAPIURL string
	serviceKey string
	logger     *zap.Logger
}

// NewMembersHandler creates a new MembersHandler.
func NewMembersHandler(rbacSvc *rbac.Service, db *ent.Client, authAPIURL, serviceKey string, logger *zap.Logger) *MembersHandler {
	return &MembersHandler{
		rbacSvc:    rbacSvc,
		db:         db,
		authAPIURL: strings.TrimRight(authAPIURL, "/"),
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
	You       bool      `json:"you"`
	CreatedAt time.Time `json:"created_at"`
}

// inviteRoles maps the MarketFlow role picked in the UI to the auth-api roles
// that the JIT sync maps back to it (rbac.mapGlobalRoleToMarketflowRole).
var inviteRoles = map[string][]string{
	rbac.RoleTenantAdmin: {"admin"},
	"fbo_owner":          {"member"},
	"viewer":             {"viewer"},
}

// caller is who is asking: their local user (nil when not yet synced) and whether they may manage the team.
type caller struct {
	localID  uuid.UUID
	canAdmin bool
}

func (h *MembersHandler) who(r *http.Request, tenantID uuid.UUID) caller {
	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok || claims == nil {
		return caller{}
	}
	var c caller
	if authID, err := claims.UserID(); err == nil {
		if u, err := h.rbacSvc.UserByAuthID(r.Context(), tenantID, authID); err == nil && u != nil {
			c.localID = u.ID
			c.canAdmin, _ = h.rbacSvc.HasRole(r.Context(), tenantID, u.ID, rbac.RoleTenantAdmin)
		}
	}
	if claims.IsPlatformOwner || claims.IsService {
		c.canAdmin = true
	}
	return c
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
	me := h.who(r, tenantID)
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
			You:       u.ID == me.localID,
			CreatedAt: u.CreatedAt,
		})
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{
		"members":    members,
		"total":      len(members),
		"can_manage": me.canAdmin,
		"roles":      rbac.AssignableRoles,
	})
}

type inviteRequest struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
	Role  string `json:"role"`
}

// InviteMember handles POST /api/v1/tenant/members/invite.
// Adds the person to the tenant in auth-api, which owns identity and emails a
// sign-in link to a new account. Only email, name and an allowed role are sent:
// the s2s route acts with platform rights, so the body is never the client's.
// @Summary Invite a new team member
// @Tags Tenant
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 201 {object} map[string]interface{}
// @Security bearerAuth
// @Router /tenant/members/invite [post]
func (h *MembersHandler) InviteMember(w http.ResponseWriter, r *http.Request) {
	tenantID, err := uuid.Parse(r.Header.Get("X-Tenant-ID"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "X-Tenant-ID required", nil)
		return
	}
	if !h.who(r, tenantID).canAdmin {
		httpapi.Error(w, http.StatusForbidden, "forbidden", "only a MarketFlow admin of this business can invite people", nil)
		return
	}
	var req inviteRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
		return
	}
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	if addr, err := mail.ParseAddress(req.Email); err != nil || addr.Address != req.Email {
		httpapi.Error(w, http.StatusBadRequest, "invalid_email", "enter a valid email address", nil)
		return
	}
	authRoles, ok := inviteRoles[req.Role]
	if !ok {
		httpapi.Error(w, http.StatusBadRequest, "invalid_role", "role must be one of marketflow_admin, fbo_owner or viewer", nil)
		return
	}

	// merge_roles keeps roles the person already has in other apps of this business (POS staff, for example).
	payload, _ := json.Marshal(map[string]any{
		"email":       req.Email,
		"name":        strings.TrimSpace(req.Name),
		"roles":       authRoles,
		"merge_roles": true,
	})
	url := fmt.Sprintf("%s/api/v1/s2s/tenants/%s/members", h.authAPIURL, tenantID)
	out, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "invite_failed", "failed to create request", nil)
		return
	}
	out.Header.Set("Content-Type", "application/json")
	out.Header.Set("X-API-Key", h.serviceKey)

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(out)
	if err != nil {
		h.logger.Error("auth-api invite call failed", zap.Error(err))
		httpapi.Error(w, http.StatusBadGateway, "invite_failed", "the sign-in service is unavailable, try again shortly", nil)
		return
	}
	defer resp.Body.Close()
	var result struct {
		UserID       string `json:"user_id"`
		TempPassword string `json:"temp_password"`
		Error        string `json:"error"`
		Message      string `json:"message"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&result)
	if resp.StatusCode >= 300 {
		h.logger.Warn("auth-api refused invite", zap.Int("status", resp.StatusCode), zap.String("error", result.Error))
		msg := result.Message
		if msg == "" {
			msg = "the invite was not accepted"
		}
		// A 401 or 403 here is about this service's key, not the person, so it must not
		// reach the browser as one (the UI signs out on a 401).
		status := resp.StatusCode
		if status >= 500 || status == http.StatusUnauthorized || status == http.StatusForbidden {
			status = http.StatusBadGateway
			msg = "the sign-in service could not add this person right now"
		}
		httpapi.Error(w, status, "invite_failed", msg, nil)
		return
	}
	audit.Tenant(r.Context(), h.db, h.logger, tenantID, "team.member_invited", "user", nil, map[string]any{"role": req.Role, "new_account": result.TempPassword != ""})
	// A new account gets a sign-in link by email and must change its password on first sign-in.
	// auth-api returns the temporary password once so the admin can pass it on; it is never logged or stored here.
	resp201 := map[string]any{
		"user_id":     result.UserID,
		"email":       req.Email,
		"role":        req.Role,
		"new_account": result.TempPassword != "",
	}
	if result.TempPassword != "" {
		resp201["temp_password"] = result.TempPassword
	}
	httpapi.JSON(w, http.StatusCreated, resp201)
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
	me := h.who(r, tenantID)
	if !me.canAdmin {
		httpapi.Error(w, http.StatusForbidden, "forbidden", "only a MarketFlow admin of this business can change roles", nil)
		return
	}
	if userID == me.localID {
		httpapi.Error(w, http.StatusConflict, "own_role", "you cannot change your own role; ask another admin", nil)
		return
	}
	var req struct {
		Role string `json:"role"`
	}
	if err := httpapi.DecodeJSON(r, &req); err != nil || !slices.Contains(rbac.AssignableRoles, req.Role) {
		httpapi.Error(w, http.StatusBadRequest, "invalid_role", "role must be one of marketflow_admin, fbo_owner or viewer", nil)
		return
	}
	if err := h.rbacSvc.UpdateUserRole(r.Context(), tenantID, userID, req.Role, me.localID); err != nil {
		h.logger.Warn("update member role", zap.Error(err))
		httpapi.Error(w, http.StatusBadRequest, "update_failed", "could not change the role", nil)
		return
	}
	audit.Tenant(r.Context(), h.db, h.logger, tenantID, "team.member_role_changed", "user", &userID, map[string]any{"role": req.Role})
	httpapi.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

// RemoveMember handles DELETE /api/v1/tenant/members/{userId}.
// It deactivates the person in MarketFlow; their sign-in account stays.
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
	me := h.who(r, tenantID)
	if !me.canAdmin {
		httpapi.Error(w, http.StatusForbidden, "forbidden", "only a MarketFlow admin of this business can remove people", nil)
		return
	}
	if userID == me.localID {
		httpapi.Error(w, http.StatusConflict, "remove_self", "you cannot remove yourself; ask another admin", nil)
		return
	}
	if err := h.rbacSvc.DeactivateUser(r.Context(), tenantID, userID); err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "remove_failed", "failed to remove member", nil)
		return
	}
	audit.Tenant(r.Context(), h.db, h.logger, tenantID, "team.member_removed", "user", &userID, nil)
	httpapi.JSON(w, http.StatusOK, map[string]any{"ok": true})
}
