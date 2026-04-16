package admin

import (
	"net/http"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"go.uber.org/zap"
)

// TenantsHandler lists / manages tenants for the platform console.
type TenantsHandler struct {
	db     *ent.Client
	logger *zap.Logger
}

func NewTenantsHandler(db *ent.Client, logger *zap.Logger) *TenantsHandler {
	return &TenantsHandler{db: db, logger: logger.Named("admin.tenants")}
}

// List returns a minimal tenant directory for the platform console and the
// tenant switcher in the UI. Platform owners only (gated by route middleware).
//
// @Summary List tenants (platform)
// @Tags Admin - Tenants
// @Produce json
// @Param active query bool false "Filter by active status"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /admin/tenants [get]
func (h *TenantsHandler) List(w http.ResponseWriter, r *http.Request) {
	q := h.db.Tenant.Query()
	if r.URL.Query().Get("active") == "true" {
		q = q.Where(tenant.StatusEQ("active"))
	}
	rows, err := q.Order(ent.Asc(tenant.FieldName)).All(r.Context())
	if err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "query_failed", err.Error(), nil)
		return
	}
	type item struct {
		ID           string `json:"id"`
		Slug         string `json:"slug"`
		Name         string `json:"name"`
		BusinessType string `json:"business_type,omitempty"`
		Status       string `json:"status,omitempty"`
		Country      string `json:"country,omitempty"`
		CreatedAt    string `json:"created_at"`
	}
	out := make([]item, 0, len(rows))
	for _, t := range rows {
		it := item{
			ID:           t.ID.String(),
			Slug:         t.Slug,
			Name:         t.Name,
			BusinessType: t.BusinessType,
			Status:       t.Status,
			CreatedAt:    t.CreatedAt.Format("2006-01-02T15:04:05Z"),
		}
		if t.Country != nil {
			it.Country = *t.Country
		}
		out = append(out, it)
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"tenants": out, "total": len(out)})
}
