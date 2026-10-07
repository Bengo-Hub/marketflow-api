package contacts

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/contact"
	"github.com/bengobox/marketflow-api/internal/ent/predicate"
	"github.com/bengobox/marketflow-api/internal/httpapi"
)

// SegmentFilter returns the SQL predicates selecting a segment's member contacts (the segments
// module's rule engine), so a broadcast to a segment reaches exactly its members.
type SegmentFilter func(ctx context.Context, tenantID, segmentID uuid.UUID) ([]predicate.Contact, error)

// WithSegmentFilter lets the reach endpoint narrow to a segment.
func WithSegmentFilter(f SegmentFilter) func(*Handler) {
	return func(h *Handler) { h.segmentFilter = f }
}

// consentRecorded is true when the contact's marketing consent has a recorded source (signup or
// checkout opt-in, a form). Contacts from before consent was recorded have none, or the
// "legacy_unverified" tag, and only get marketing when the sender attests they agreed.
func consentRecorded(c *ent.Contact) bool {
	src, _ := c.Metadata["consent_source"].(string)
	src = strings.TrimSpace(src)
	return src != "" && src != "legacy_unverified"
}

// Reach pages through a tenant's reachable contacts (an email or a phone) in id order, optionally
// narrowed by extra predicates (a segment). Keyset paging: pass the last id as after.
func (s *Service) Reach(ctx context.Context, tenantID uuid.UUID, filter []predicate.Contact, after *uuid.UUID, limit int) ([]*ent.Contact, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := s.client.Contact.Query().
		Where(
			contact.TenantID(tenantID),
			contact.Not(contact.And(contact.FirstNameEqualFold("Walk-in"), contact.LastNameEqualFold("Customer"))),
			contact.Or(
				contact.And(contact.EmailNotNil(), contact.EmailNEQ("")),
				contact.And(contact.PhoneNotNil(), contact.PhoneNEQ("")),
			),
		).
		Where(filter...)
	if after != nil {
		q = q.Where(contact.IDGT(*after))
	}
	return q.Order(ent.Asc(contact.FieldID)).Limit(limit).All(ctx)
}

// ReachInternal handles GET /api/v1/internal/contacts/reach?tenant_id=&segment_id=&after=&limit=
// S2S only (X-API-Key). notifications-api pages through a tenant's customers for a broadcast:
// names, email, phone and the marketing consent flags it must honour. Keyset paging by id;
// next is "" on the last page.
func (h *Handler) ReachInternal(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tenantID, err := uuid.Parse(q.Get("tenant_id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "missing_tenant", "tenant_id query parameter required", nil)
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	var after *uuid.UUID
	if a := strings.TrimSpace(q.Get("after")); a != "" {
		id, err := uuid.Parse(a)
		if err != nil {
			httpapi.Error(w, http.StatusBadRequest, "invalid_after", "after must be a contact id", nil)
			return
		}
		after = &id
	}
	var filter []predicate.Contact
	if s := strings.TrimSpace(q.Get("segment_id")); s != "" {
		segID, err := uuid.Parse(s)
		if err != nil {
			httpapi.Error(w, http.StatusBadRequest, "invalid_segment", "segment_id must be a uuid", nil)
			return
		}
		if h.segmentFilter == nil {
			httpapi.Error(w, http.StatusNotImplemented, "segments_unavailable", "segment filtering is not available", nil)
			return
		}
		filter, err = h.segmentFilter(r.Context(), tenantID, segID)
		if err != nil {
			httpapi.Error(w, http.StatusBadRequest, "invalid_segment", err.Error(), nil)
			return
		}
	}

	rows, err := h.service.Reach(r.Context(), tenantID, filter, after, limit)
	if err != nil && !errors.Is(err, context.Canceled) {
		h.logger.Error("s2s contact reach failed", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "reach_failed", "failed to list contacts", nil)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, c := range rows {
		country, _ := c.Metadata["country"].(string)
		row := map[string]any{
			"id":               c.ID,
			"first_name":       deref(c.FirstName),
			"last_name":        deref(c.LastName),
			"email":            deref(c.Email),
			"phone":            deref(c.Phone),
			"country":          country,
			"subscribed_email": c.SubscribedEmail,
			"subscribed_sms":   c.SubscribedSms,
			"consent_recorded": consentRecorded(c),
		}
		out = append(out, row)
	}
	next := ""
	if len(rows) == limit {
		next = rows[len(rows)-1].ID.String()
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"data": out, "next": next})
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
