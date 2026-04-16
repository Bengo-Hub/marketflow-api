// Package audit serves read-only audit log endpoints for the platform console
// and tenant dashboards.
package audit

import (
	"net/http"
	"strconv"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/auditlog"
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
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := h.db.AuditLog.Query().Where(auditlog.ScopeEQ("platform"))
	if event != "" {
		q = q.Where(auditlog.EventEQ(event))
	}
	rows, err := q.Order(ent.Desc(auditlog.FieldOccurredAt)).Limit(limit).All(r.Context())
	if err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "query_failed", err.Error(), nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{
		"data":  rows,
		"total": len(rows),
	})
}

// ListTenant returns tenant-scope audit events for the authenticated tenant.
// @Summary List tenant audit events
// @Tags Analytics - Audit
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param event query string false "Filter by event name"
// @Param limit query int false "Max rows (default 50)"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /analytics/audit [get]
func (h *Handler) ListTenant(w http.ResponseWriter, r *http.Request) {
	tenantID, err := uuid.Parse(r.Header.Get("X-Tenant-ID"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing X-Tenant-ID", nil)
		return
	}
	event := r.URL.Query().Get("event")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := h.db.AuditLog.Query().Where(auditlog.TenantIDEQ(tenantID))
	if event != "" {
		q = q.Where(auditlog.EventEQ(event))
	}
	rows, err := q.Order(ent.Desc(auditlog.FieldOccurredAt)).Limit(limit).All(r.Context())
	if err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "query_failed", err.Error(), nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{
		"data":  rows,
		"total": len(rows),
	})
}
