package testimonials

import (
	"net/http"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/modules/audit"
)

// mediaLinkTTL is how long a signed link to a customer's photo or video lasts in a response.
const (
	adminMediaTTL  = time.Hour
	publicMediaTTL = 24 * time.Hour
)

// editAllowed keeps a customer's and a website's words as given: only the tenant's own
// testimonials can have their author or words edited, and only an approved one can be published.
func editAllowed(existing *ent.Testimonial, req upsertRequest) (string, string) {
	if existing.Source != "" && existing.Source != SourceManual {
		if (req.AuthorName != nil && *req.AuthorName != existing.AuthorName) || (req.Body != nil && *req.Body != existing.Body) {
			return "words_locked", "a customer's or website testimonial is shown as given: its author and words cannot be edited"
		}
		if req.Rating != nil && *req.Rating != existing.Rating {
			return "words_locked", "a customer's rating cannot be changed"
		}
	}
	if req.IsPublished != nil && *req.IsPublished && existing.Status != StatusApproved && existing.Status != "" {
		return "not_approved", "approve the testimonial before publishing it"
	}
	return "", ""
}

// mediaLink signs a link to the testimonial's photo or video, or "" without one.
func (h *Handler) mediaLink(t *ent.Testimonial, ttl time.Duration) string {
	key, _ := t.Metadata["media_key"].(string)
	if key == "" || h.store == nil || !h.store.Enabled() {
		return ""
	}
	u, err := h.store.DownloadURL(key, "", ttl)
	if err != nil {
		return ""
	}
	return u
}

// adminView is the testimonial for the tenant's admins: review state, source, consent, media and
// the submitter's contact details.
func (h *Handler) adminView(t *ent.Testimonial) map[string]any {
	out := serializeTestimonial(t)
	out["status"] = t.Status
	out["source"] = t.Source
	for _, k := range []string{"consent", "consent_at", "submitter_email", "submitter_phone", "media_kind", "source_url", "reviewed_at", "reviewed_by"} {
		if v, ok := t.Metadata[k]; ok {
			out[k] = v
		}
	}
	if u := h.mediaLink(t, adminMediaTTL); u != "" {
		out["media_url"] = u
	}
	return out
}

// publicView is what a public page shows: the words, author, rating and, when the customer gave
// it with their consent, their photo or video. Never their contact details.
func (h *Handler) publicView(t *ent.Testimonial) map[string]any {
	out := map[string]any{
		"id":            t.ID,
		"author_name":   t.AuthorName,
		"author_role":   t.AuthorRole,
		"body":          t.Body,
		"avatar_url":    t.AvatarURL,
		"rating":        ratingOf(t),
		"display_order": t.DisplayOrder,
		"is_published":  t.IsPublished,
		"source":        t.Source,
		"created_at":    t.CreatedAt,
	}
	if consent, _ := t.Metadata["consent"].(bool); consent || t.Source == SourceManual {
		if u := h.mediaLink(t, publicMediaTTL); u != "" {
			out["media_url"] = u
			out["media_kind"] = t.Metadata["media_kind"]
		}
	}
	return out
}

type reviewRequest struct {
	// Publish shows an approved testimonial at once (default true).
	Publish *bool `json:"publish"`
}

// Approve handles POST /api/v1/testimonials/{id}/approve. A tenant admin or platform admin
// approves a customer's or website testimonial; with publish (the default) it goes on the public
// pages and into the testimonial ad inputs.
// @Summary Approve a testimonial
// @Tags Testimonials
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Testimonial ID"
// @Param request body reviewRequest false "publish (default true)"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /testimonials/{id}/approve [post]
func (h *Handler) Approve(w http.ResponseWriter, r *http.Request) { h.review(w, r, true) }

// Reject handles POST /api/v1/testimonials/{id}/reject: it is never shown.
// @Summary Reject a testimonial
// @Tags Testimonials
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Testimonial ID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /testimonials/{id}/reject [post]
func (h *Handler) Reject(w http.ResponseWriter, r *http.Request) { h.review(w, r, false) }

func (h *Handler) review(w http.ResponseWriter, r *http.Request, approve bool) {
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
	var req reviewRequest
	_ = httpapi.DecodeJSON(r, &req)
	publish := req.Publish == nil || *req.Publish
	t, err := Review(r.Context(), h.db, tenantID, id, approve, publish, reviewerOf(r))
	if err != nil {
		if ent.IsNotFound(err) {
			httpapi.Error(w, http.StatusNotFound, "not_found", "testimonial not found", nil)
			return
		}
		h.logger.Error("review testimonial", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "save_failed", "could not save the review", nil)
		return
	}
	action := "testimonial.rejected"
	if approve {
		action = "testimonial.approved"
	}
	audit.Tenant(r.Context(), h.db, h.logger, tenantID, action, "testimonial", &t.ID, map[string]any{"is_published": t.IsPublished, "source": t.Source})
	httpapi.JSON(w, http.StatusOK, h.adminView(t))
}

// reviewerOf is the signed-in reviewer (email, else user id) from the verified token.
func reviewerOf(r *http.Request) string {
	if c, ok := authclient.ClaimsFromContext(r.Context()); ok && c != nil {
		if c.Email != "" {
			return c.Email
		}
		return c.Subject
	}
	return ""
}

// ratingOf is the testimonial's rating as given: 0 when its source gave none (a website quote
// without stars), so no rating is ever shown or used that the customer did not give.
func ratingOf(t *ent.Testimonial) int {
	if unknown, _ := t.Metadata["rating_unknown"].(bool); unknown {
		return 0
	}
	return t.Rating
}
