package backup

import (
	"fmt"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/httpapi"
)

// Handler exposes the tenant-scoped backup HTTP endpoints (this tenant's data only). The
// artifact is a gzipped-JSON export of the tenant's rows, tracked in the `backups` table.
// Tenant is resolved from the X-Tenant-ID header (the marketflow convention); the route is
// mounted inside the authenticated group.
type Handler struct {
	svc           *Service
	log           *zap.Logger
	retentionDays int
}

// NewHandler builds the backup HTTP handler.
func NewHandler(svc *Service, retentionDays int, log *zap.Logger) *Handler {
	if retentionDays <= 0 {
		retentionDays = DefaultRetentionDays
	}
	return &Handler{svc: svc, log: log.Named("backup.Handler"), retentionDays: retentionDays}
}

func tenantFromRequest(r *http.Request) (uuid.UUID, error) {
	h := r.Header.Get("X-Tenant-ID")
	if h == "" {
		return uuid.UUID{}, fmt.Errorf("X-Tenant-ID header is required")
	}
	return uuid.Parse(h)
}

// List handles GET /api/v1/backups.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	tenantID, err := tenantFromRequest(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	items, err := h.svc.List(r.Context(), tenantID)
	if err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "internal_error", "failed to list backups", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"backups": items})
}

// Create handles POST /api/v1/backups.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	tenantID, err := tenantFromRequest(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	info, err := h.svc.Generate(r.Context(), tenantID)
	if err != nil {
		h.log.Error("generate backup", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "backup_failed", "failed to generate backup", nil)
		return
	}
	httpapi.JSON(w, http.StatusCreated, info)
}

// Churn handles POST /api/v1/backups/churn — manual retention cleanup (deletes files +
// tracking rows older than the configured window). The daily scheduler runs this too.
func (h *Handler) Churn(w http.ResponseWriter, r *http.Request) {
	removed, err := h.svc.Churn(r.Context(), h.retentionDays)
	if err != nil {
		h.log.Error("churn backups", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "churn_failed", "failed to churn backups", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"removed": removed, "retention_days": h.retentionDays})
}

// Download handles GET /api/v1/backups/{name}/download.
func (h *Handler) Download(w http.ResponseWriter, r *http.Request) {
	tenantID, err := tenantFromRequest(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	name := chi.URLParam(r, "name")
	rc, err := h.svc.Open(tenantID, name)
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "not_found", "backup not found", nil)
		return
	}
	defer func() { _ = rc.Close() }()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, name))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

// Delete handles DELETE /api/v1/backups/{name}.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	tenantID, err := tenantFromRequest(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	if err := h.svc.Delete(r.Context(), tenantID, chi.URLParam(r, "name")); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "delete_failed", err.Error(), nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
