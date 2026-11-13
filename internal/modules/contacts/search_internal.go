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
func (s *Service) Search(ctx context.Context, tenantID uuid.UUID, q string, limit, offset int, contactTypes ...string) ([]*ent.Contact, int, error) {
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
	if m := matchQuery(q); m != nil {
		base = base.Where(m)
	}
	if len(contactTypes) > 0 {
		base = base.Where(contact.ContactTypeIn(contactTypes...))
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
	// contact_type (optional, comma list, e.g. "customer") narrows the book. Treasury's Customers
	// page asks for customers only, so marketing leads stay behind MarketFlow's own permissions
	// (S2-8 review M-2). Empty lists every type, as the pos picker expects.
	results, total, err := h.service.Search(r.Context(), tenantID, q, limit, offset, contactTypesParam(r.URL.Query().Get("contact_type"))...)
	if err != nil {
		h.logger.Error("s2s contact search failed", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "search_failed", "failed to search contacts", nil)
		return
	}

	out := make([]map[string]any, 0, len(results))
	for _, c := range results {
		out = append(out, map[string]any{
			"id":           c.ID,
			"first_name":   c.FirstName,
			"last_name":    c.LastName,
			"email":        c.Email,
			"phone":        c.Phone,
			"contact_type": c.ContactType,
			"created_at":   c.CreatedAt,
		})
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"data": out, "total": total})
}

// contactTypesParam splits a comma list of contact types, dropping blanks.
func contactTypesParam(v string) []string {
	var out []string
	for _, t := range strings.Split(v, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}
