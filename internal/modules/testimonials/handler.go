// Package testimonials provides CRUD for FBO success stories rendered on
// /p/{slug}. Tenant-scoped via X-Tenant-ID header (same pattern as other
// tenant-owned content) and exposed publicly via the /public/profile/{slug}
// JSON response.
package testimonials

import (
	"context"
	"net/http"
	"strings"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/testimonial"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Handler serves tenant-scoped testimonial CRUD.
type Handler struct {
	db     *ent.Client
	logger *zap.Logger
}

func NewHandler(db *ent.Client, logger *zap.Logger) *Handler {
	return &Handler{db: db, logger: logger.Named("testimonials")}
}

type upsertRequest struct {
	AuthorName   string `json:"author_name"`
	AuthorRole   string `json:"author_role,omitempty"`
	Body         string `json:"body"`
	AvatarURL    string `json:"avatar_url,omitempty"`
	Rating       int    `json:"rating,omitempty"`
	DisplayOrder int    `json:"display_order,omitempty"`
	IsPublished  *bool  `json:"is_published,omitempty"`
}

func tenantIDFromHeader(r *http.Request) (uuid.UUID, error) {
	return uuid.Parse(r.Header.Get("X-Tenant-ID"))
}

// List handles GET /api/v1/testimonials.
// @Summary List tenant testimonials
// @Tags Testimonials
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /testimonials [get]
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	tenantID, err := tenantIDFromHeader(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "X-Tenant-ID required", nil)
		return
	}
	rows, err := h.db.Testimonial.Query().
		Where(testimonial.TenantIDEQ(tenantID)).
		Order(ent.Asc(testimonial.FieldDisplayOrder), ent.Desc(testimonial.FieldCreatedAt)).
		All(r.Context())
	if err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "query_failed", err.Error(), nil)
		return
	}
	out := make([]map[string]any, len(rows))
	for i, t := range rows {
		out[i] = serializeTestimonial(t)
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"data": out})
}

// Create handles POST /api/v1/testimonials.
// @Summary Create a testimonial
// @Tags Testimonials
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param request body upsertRequest true "Testimonial payload"
// @Success 201 {object} map[string]interface{}
// @Security bearerAuth
// @Router /testimonials [post]
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	tenantID, err := tenantIDFromHeader(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "X-Tenant-ID required", nil)
		return
	}
	var req upsertRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
		return
	}
	req.AuthorName = strings.TrimSpace(req.AuthorName)
	req.Body = strings.TrimSpace(req.Body)
	if req.AuthorName == "" || req.Body == "" {
		httpapi.Error(w, http.StatusBadRequest, "missing_fields", "author_name and body are required", nil)
		return
	}
	rating := req.Rating
	if rating <= 0 {
		rating = 5
	}
	if rating > 5 {
		rating = 5
	}
	pub := true
	if req.IsPublished != nil {
		pub = *req.IsPublished
	}
	builder := h.db.Testimonial.Create().
		SetTenantID(tenantID).
		SetAuthorName(req.AuthorName).
		SetBody(req.Body).
		SetRating(rating).
		SetDisplayOrder(req.DisplayOrder).
		SetIsPublished(pub)
	if req.AuthorRole != "" {
		builder = builder.SetAuthorRole(req.AuthorRole)
	}
	if req.AvatarURL != "" {
		builder = builder.SetAvatarURL(req.AvatarURL)
	}
	t, err := builder.Save(r.Context())
	if err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "save_failed", err.Error(), nil)
		return
	}
	httpapi.JSON(w, http.StatusCreated, serializeTestimonial(t))
}

// Update handles PATCH /api/v1/testimonials/{id}.
// @Summary Update a testimonial
// @Tags Testimonials
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Testimonial ID"
// @Param request body upsertRequest true "Partial update"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /testimonials/{id} [patch]
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	tenantID, err := tenantIDFromHeader(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "X-Tenant-ID required", nil)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "testimonial id must be a UUID", nil)
		return
	}
	existing, err := h.db.Testimonial.Query().
		Where(testimonial.IDEQ(id), testimonial.TenantIDEQ(tenantID)).
		Only(r.Context())
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "not_found", "testimonial not found", nil)
		return
	}
	var req upsertRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
		return
	}
	builder := existing.Update()
	if s := strings.TrimSpace(req.AuthorName); s != "" {
		builder = builder.SetAuthorName(s)
	}
	if s := strings.TrimSpace(req.Body); s != "" {
		builder = builder.SetBody(s)
	}
	if req.AuthorRole != "" {
		builder = builder.SetAuthorRole(req.AuthorRole)
	}
	if req.AvatarURL != "" {
		builder = builder.SetAvatarURL(req.AvatarURL)
	}
	if req.Rating > 0 && req.Rating <= 5 {
		builder = builder.SetRating(req.Rating)
	}
	builder = builder.SetDisplayOrder(req.DisplayOrder)
	if req.IsPublished != nil {
		builder = builder.SetIsPublished(*req.IsPublished)
	}
	t, err := builder.Save(r.Context())
	if err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "save_failed", err.Error(), nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, serializeTestimonial(t))
}

// Delete handles DELETE /api/v1/testimonials/{id}.
// @Summary Delete a testimonial
// @Tags Testimonials
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Testimonial ID"
// @Success 204
// @Security bearerAuth
// @Router /testimonials/{id} [delete]
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	tenantID, err := tenantIDFromHeader(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "X-Tenant-ID required", nil)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "testimonial id must be a UUID", nil)
		return
	}
	n, err := h.db.Testimonial.Delete().
		Where(testimonial.IDEQ(id), testimonial.TenantIDEQ(tenantID)).
		Exec(r.Context())
	if err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "delete_failed", err.Error(), nil)
		return
	}
	if n == 0 {
		httpapi.Error(w, http.StatusNotFound, "not_found", "testimonial not found", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PublicListForTenant returns the published testimonials for a tenant, used
// by the /public/profile/{slug} handler. No auth.
func (h *Handler) PublicListForTenant(ctx context.Context, tenantID uuid.UUID, limit int) ([]map[string]any, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	rows, err := h.db.Testimonial.Query().
		Where(
			testimonial.TenantIDEQ(tenantID),
			testimonial.IsPublishedEQ(true),
		).
		Order(ent.Asc(testimonial.FieldDisplayOrder), ent.Desc(testimonial.FieldCreatedAt)).
		Limit(limit).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, len(rows))
	for i, t := range rows {
		out[i] = serializeTestimonial(t)
	}
	return out, nil
}

func serializeTestimonial(t *ent.Testimonial) map[string]any {
	return map[string]any{
		"id":            t.ID,
		"author_name":   t.AuthorName,
		"author_role":   t.AuthorRole,
		"body":          t.Body,
		"avatar_url":    t.AvatarURL,
		"rating":        t.Rating,
		"display_order": t.DisplayOrder,
		"is_published":  t.IsPublished,
		"created_at":    t.CreatedAt,
		"updated_at":    t.UpdatedAt,
	}
}
