// Package testimonials provides CRUD for FBO success stories rendered on
// /p/{slug}. Tenant-scoped via X-Tenant-ID header (same pattern as other
// tenant-owned content) and exposed publicly via the /public/profile/{slug}
// JSON response.
package testimonials

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/Bengo-Hub/pagination"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/testimonial"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/modules/audit"
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

// upsertRequest is the body for create and update. On update every field is
// optional: a field left out keeps its value, and an empty author_role or
// avatar_url clears it.
type upsertRequest struct {
	AuthorName   *string `json:"author_name,omitempty"`
	AuthorRole   *string `json:"author_role,omitempty"`
	Body         *string `json:"body,omitempty"`
	AvatarURL    *string `json:"avatar_url,omitempty"`
	Rating       *int    `json:"rating,omitempty"`
	DisplayOrder *int    `json:"display_order,omitempty"`
	IsPublished  *bool   `json:"is_published,omitempty"`
}

// Limits match the ent schema, so bad input is a 400 rather than a failed save.
const (
	maxNameLen   = 120
	maxRoleLen   = 120
	maxAvatarLen = 512
	maxBodyLen   = 2000
)

// trimmed returns the trimmed value of an optional string field.
func trimmed(s *string) *string {
	if s == nil {
		return nil
	}
	v := strings.TrimSpace(*s)
	return &v
}

// validate checks the fields that are present. It returns a code and message for a 400.
func (req *upsertRequest) validate() (string, string) {
	req.AuthorName, req.AuthorRole, req.Body, req.AvatarURL = trimmed(req.AuthorName), trimmed(req.AuthorRole), trimmed(req.Body), trimmed(req.AvatarURL)
	switch {
	case req.AuthorName != nil && utf8.RuneCountInString(*req.AuthorName) > maxNameLen:
		return "invalid_author_name", "author_name is longer than 120 characters"
	case req.AuthorRole != nil && utf8.RuneCountInString(*req.AuthorRole) > maxRoleLen:
		return "invalid_author_role", "author_role is longer than 120 characters"
	case req.Body != nil && utf8.RuneCountInString(*req.Body) > maxBodyLen:
		return "invalid_body", "body is longer than 2000 characters"
	case req.Rating != nil && (*req.Rating < 1 || *req.Rating > 5):
		return "invalid_rating", "rating must be between 1 and 5"
	case req.AvatarURL != nil && *req.AvatarURL != "" && !webURL(*req.AvatarURL):
		return "invalid_avatar_url", "avatar_url must be an http(s) link of at most 512 characters"
	}
	return "", ""
}

// webURL reports whether s is an absolute http(s) link that fits the column.
func webURL(s string) bool {
	if len(s) > maxAvatarLen {
		return false
	}
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
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
	p := pagination.Parse(r)
	q := h.db.Testimonial.Query().
		Where(testimonial.TenantIDEQ(tenantID)).
		Order(ent.Asc(testimonial.FieldDisplayOrder), ent.Desc(testimonial.FieldCreatedAt))
	total, _ := q.Clone().Count(r.Context())
	rows, err := q.Limit(p.Limit).Offset(p.Offset).All(r.Context())
	if err != nil {
		h.logger.Error("list testimonials", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "query_failed", "could not load testimonials", nil)
		return
	}
	out := make([]map[string]any, len(rows))
	for i, t := range rows {
		out[i] = serializeTestimonial(t)
	}
	httpapi.JSON(w, http.StatusOK, pagination.NewResponse(out, total, p))
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
	if code, msg := req.validate(); code != "" {
		httpapi.Error(w, http.StatusBadRequest, code, msg, nil)
		return
	}
	if req.AuthorName == nil || *req.AuthorName == "" || req.Body == nil || *req.Body == "" {
		httpapi.Error(w, http.StatusBadRequest, "missing_fields", "author_name and body are required", nil)
		return
	}
	builder := h.db.Testimonial.Create().
		SetTenantID(tenantID).
		SetAuthorName(*req.AuthorName).
		SetBody(*req.Body)
	if req.Rating != nil {
		builder = builder.SetRating(*req.Rating)
	}
	if req.DisplayOrder != nil {
		builder = builder.SetDisplayOrder(*req.DisplayOrder)
	}
	if req.IsPublished != nil {
		builder = builder.SetIsPublished(*req.IsPublished)
	}
	if req.AuthorRole != nil && *req.AuthorRole != "" {
		builder = builder.SetAuthorRole(*req.AuthorRole)
	}
	if req.AvatarURL != nil && *req.AvatarURL != "" {
		builder = builder.SetAvatarURL(*req.AvatarURL)
	}
	t, err := builder.Save(r.Context())
	if err != nil {
		h.logger.Error("create testimonial", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "save_failed", "could not save the testimonial", nil)
		return
	}
	audit.Tenant(r.Context(), h.db, h.logger, tenantID, "testimonial.created", "testimonial", &t.ID, map[string]any{"is_published": t.IsPublished})
	httpapi.JSON(w, http.StatusCreated, serializeTestimonial(t))
}

// Update handles PATCH /api/v1/testimonials/{id}. Fields left out keep their values.
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
	var req upsertRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
		return
	}
	if code, msg := req.validate(); code != "" {
		httpapi.Error(w, http.StatusBadRequest, code, msg, nil)
		return
	}
	if (req.AuthorName != nil && *req.AuthorName == "") || (req.Body != nil && *req.Body == "") {
		httpapi.Error(w, http.StatusBadRequest, "missing_fields", "author_name and body cannot be empty", nil)
		return
	}
	existing, err := h.db.Testimonial.Query().
		Where(testimonial.IDEQ(id), testimonial.TenantIDEQ(tenantID)).
		Only(r.Context())
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "not_found", "testimonial not found", nil)
		return
	}
	builder := existing.Update()
	if req.AuthorName != nil {
		builder = builder.SetAuthorName(*req.AuthorName)
	}
	if req.Body != nil {
		builder = builder.SetBody(*req.Body)
	}
	if req.AuthorRole != nil {
		if *req.AuthorRole == "" {
			builder = builder.ClearAuthorRole()
		} else {
			builder = builder.SetAuthorRole(*req.AuthorRole)
		}
	}
	if req.AvatarURL != nil {
		if *req.AvatarURL == "" {
			builder = builder.ClearAvatarURL()
		} else {
			builder = builder.SetAvatarURL(*req.AvatarURL)
		}
	}
	if req.Rating != nil {
		builder = builder.SetRating(*req.Rating)
	}
	if req.DisplayOrder != nil {
		builder = builder.SetDisplayOrder(*req.DisplayOrder)
	}
	if req.IsPublished != nil {
		builder = builder.SetIsPublished(*req.IsPublished)
	}
	t, err := builder.Save(r.Context())
	if err != nil {
		h.logger.Error("update testimonial", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "save_failed", "could not save the testimonial", nil)
		return
	}
	audit.Tenant(r.Context(), h.db, h.logger, tenantID, "testimonial.updated", "testimonial", &t.ID, map[string]any{"is_published": t.IsPublished})
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
		h.logger.Error("delete testimonial", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "delete_failed", "could not delete the testimonial", nil)
		return
	}
	if n == 0 {
		httpapi.Error(w, http.StatusNotFound, "not_found", "testimonial not found", nil)
		return
	}
	audit.Tenant(r.Context(), h.db, h.logger, tenantID, "testimonial.deleted", "testimonial", &id, nil)
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
