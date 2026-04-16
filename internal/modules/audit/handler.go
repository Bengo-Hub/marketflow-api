// Package audit serves read-only audit log endpoints for the platform console
// and tenant dashboards.
package audit

import (
	"net/http"
	"strconv"
	"strings"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/auditlog"
	"github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Handler owns /admin/audit (platform) and /analytics/audit (tenant) endpoints.
type Handler struct {
	db     *ent.Client
	logger *zap.Logger
}

func NewHandler(db *ent.Client, logger *zap.Logger) *Handler {
	return &Handler{db: db, logger: logger.Named("audit")}
}

// ListPlatform returns platform-scope audit events, most recent first.
// @Summary List platform audit events
// @Tags Admin - Audit
// @Produce json
// @Param event query string false "Filter by event name"
// @Param limit query int false "Max rows (default 50)"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /admin/audit [get]
func (h *Handler) ListPlatform(w http.ResponseWriter, r *http.Request) {
	event := r.URL.Query().Get("event")
	limit := parseLimit(r)
	q := h.db.AuditLog.Query().Where(auditlog.ScopeEQ("platform"))
	if event != "" {
		q = q.Where(auditlog.EventEQ(event))
	}
	rows, err := q.Order(ent.Desc(auditlog.FieldOccurredAt)).Limit(limit).All(r.Context())
	if err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "query_failed", err.Error(), nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"data": rows, "total": len(rows)})
}

// ListTenant returns tenant-scope audit events for the authenticated tenant.
// @Summary List tenant audit events
// @Tags Analytics - Audit
// @Produce json
// @Param X-Tenant-ID header string false "Tenant UUID (or use X-Tenant-Slug)"
// @Param X-Tenant-Slug header string false "Tenant slug (used by platform owners acting as a tenant)"
// @Param event query string false "Filter by event name"
// @Param limit query int false "Max rows (default 50)"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /analytics/audit [get]
func (h *Handler) ListTenant(w http.ResponseWriter, r *http.Request) {
	tenantID, err := h.resolveTenant(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant",
			"no tenant context; pass X-Tenant-ID or X-Tenant-Slug", nil)
		return
	}
	event := r.URL.Query().Get("event")
	limit := parseLimit(r)
	q := h.db.AuditLog.Query().Where(auditlog.TenantIDEQ(tenantID))
	if event != "" {
		q = q.Where(auditlog.EventEQ(event))
	}
	rows, err := q.Order(ent.Desc(auditlog.FieldOccurredAt)).Limit(limit).All(r.Context())
	if err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "query_failed", err.Error(), nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"data": rows, "total": len(rows)})
}

func parseLimit(r *http.Request) int {
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if n <= 0 || n > 200 {
		return 50
	}
	return n
}

// resolveTenant picks a tenant UUID from X-Tenant-ID, X-Tenant-Slug (with DB
// lookup so platform owners can act as a tenant via /{orgSlug}/... UI routes),
// or the JWT claims fallback.
func (h *Handler) resolveTenant(r *http.Request) (uuid.UUID, error) {
	if v := strings.TrimSpace(r.Header.Get("X-Tenant-ID")); v != "" {
		return uuid.Parse(v)
	}
	ctx := r.Context()
	if slug := strings.TrimSpace(r.Header.Get("X-Tenant-Slug")); slug != "" {
		if t, err := h.db.Tenant.Query().Where(tenant.SlugEQ(slug)).Only(ctx); err == nil {
			return t.ID, nil
		}
	}
	if claims, ok := authclient.ClaimsFromContext(ctx); ok && claims != nil && claims.TenantID != "" {
		return uuid.Parse(claims.TenantID)
	}
	return uuid.Nil, errNoTenant
}

var errNoTenant = &noTenantErr{}

type noTenantErr struct{}

func (*noTenantErr) Error() string { return "no tenant context" }
