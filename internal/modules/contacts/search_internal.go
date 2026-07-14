package contacts

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/contact"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Search lists/searches a tenant's contacts for the customer directory: q == "" lists all
// (newest first), otherwise a case-insensitive substring match on name/email/phone. Returns
// the page plus the total match count for pagination.
func (s *Service) Search(ctx context.Context, tenantID uuid.UUID, q string, limit, offset int) ([]*ent.Contact, int, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	base := s.client.Contact.Query().
		Where(
			contact.TenantID(tenantID),
			contact.Not(contact.And(contact.FirstNameEqualFold("Walk-in"), contact.LastNameEqualFold("Customer"))),
		)
	if q != "" {
		base = base.Where(contact.Or(
			contact.FirstNameContainsFold(q),
			contact.LastNameContainsFold(q),
			contact.EmailContainsFold(q),
			contact.PhoneContainsFold(q),
		))
	}
	total, err := base.Clone().Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	rows, err := base.Order(ent.Desc(contact.FieldCreatedAt)).Limit(limit).Offset(offset).All(ctx)
	if err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

// SearchInternal handles GET /api/v1/internal/contacts/search?tenant_id=&q=&limit=&offset=
// S2S only — requires X-API-Key, no JWT. Lets sibling services (pos-api customer directory /
// picker) list and search the CRM — the customer master — by name/email/phone instead of only
// their own local mirrors. q is optional: empty lists the whole directory paginated. Returns
// minimal profiles only, never the full contact.
func (h *Handler) SearchInternal(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	tenantID, err := uuid.Parse(r.URL.Query().Get("tenant_id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "missing_tenant", "tenant_id query parameter required", nil)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	results, total, err := h.service.Search(r.Context(), tenantID, q, limit, offset)
	if err != nil {
		h.logger.Error("s2s contact search failed", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "search_failed", "failed to search contacts", nil)
		return
	}

	out := make([]map[string]any, 0, len(results))
	for _, c := range results {
		out = append(out, map[string]any{
			"id":         c.ID,
			"first_name": c.FirstName,
			"last_name":  c.LastName,
			"email":      c.Email,
			"phone":      c.Phone,
			"created_at": c.CreatedAt,
		})
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"data": out, "total": total})
}
