package contacts

import (
	"context"
	"fmt"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// BulkImportRow is one contact to upsert. ExternalRef is the caller's own identifier (e.g. a
// legacy system's contact id) used purely to key the response back to the caller's input row —
// it is never stored. ID, if set, is used as the new contact's primary key (useful for a
// migration that wants a stable, pre-computed id it can also feed to downstream services'
// crm_contact_id columns); when dedup matches an existing contact by phone/email, the existing
// contact's real id wins over whatever ID was requested.
type BulkImportRow struct {
	ExternalRef    string
	ID             *uuid.UUID
	Email          *string
	Phone          *string
	FirstName      *string
	LastName       *string
	ContactType    string
	LifecycleStage string
	Metadata       map[string]any
}

// BulkImportRowResult reports what happened to a single input row.
type BulkImportRowResult struct {
	ExternalRef string    `json:"external_ref"`
	ContactID   uuid.UUID `json:"contact_id"`
	Status      string    `json:"status"` // created | existing | failed
	Error       string    `json:"error,omitempty"`
}

// BulkImportResult summarises a bulk import call.
type BulkImportResult struct {
	Created  int                   `json:"created"`
	Existing int                   `json:"existing"`
	Failed   int                   `json:"failed"`
	Results  []BulkImportRowResult `json:"results"`
}

// BulkImport upserts a batch of contacts for a tenant, deduplicating by phone then email (same
// rule as Create) so re-running an import is idempotent. Never fails the whole batch for one bad
// row — each row's outcome is reported independently in the result.
func (s *Service) BulkImport(ctx context.Context, tenantID uuid.UUID, rows []BulkImportRow) (*BulkImportResult, error) {
	res := &BulkImportResult{Results: make([]BulkImportRowResult, 0, len(rows))}

	for _, row := range rows {
		id, status, err := s.upsertOne(ctx, tenantID, row)
		rr := BulkImportRowResult{ExternalRef: row.ExternalRef, Status: status}
		if err != nil {
			res.Failed++
			rr.Status = "failed"
			rr.Error = err.Error()
			s.logger.Warn("bulk import: row failed",
				zap.String("external_ref", row.ExternalRef), zap.Error(err))
		} else {
			rr.ContactID = id
			switch status {
			case "created":
				res.Created++
			case "existing":
				res.Existing++
			}
		}
		res.Results = append(res.Results, rr)
	}

	return res, nil
}

// upsertOne applies the same normalized phone-then-email dedup rule as Create (findExistingContact
// — format-agnostic, not the raw exact-match this used to do), returning the id of the contact
// that now represents this row (existing or newly created) and which of the two happened.
func (s *Service) upsertOne(ctx context.Context, tenantID uuid.UUID, row BulkImportRow) (uuid.UUID, string, error) {
	emailNorm := ""
	if row.Email != nil {
		emailNorm = normalizeEmail(*row.Email)
	}
	phoneDigits := ""
	if row.Phone != nil {
		phoneDigits = nationalSubscriberDigits(*row.Phone)
	}
	if existing, _ := s.findExistingContact(ctx, tenantID, emailNorm, phoneDigits); existing != nil {
		return existing.ID, "existing", nil
	}

	contactType := row.ContactType
	if contactType == "" {
		contactType = "customer"
	}

	builder := s.client.Contact.Create().
		SetTenantID(tenantID).
		SetContactType(contactType)
	if row.ID != nil {
		builder = builder.SetID(*row.ID)
	}
	if row.LifecycleStage != "" {
		builder = builder.SetLifecycleStage(row.LifecycleStage)
	}
	builder = builder.
		SetNillableEmail(row.Email).
		SetNillablePhone(row.Phone).
		SetNillableFirstName(row.FirstName).
		SetNillableLastName(row.LastName)
	if emailNorm != "" {
		builder = builder.SetEmailNormalized(emailNorm)
	}
	if phoneDigits != "" {
		builder = builder.SetPhoneSubscriberDigits(phoneDigits)
	}
	if row.Metadata != nil {
		builder = builder.SetMetadata(row.Metadata)
	}

	created, err := builder.Save(ctx)
	if err != nil {
		if ent.IsConstraintError(err) {
			// Lost a race against a concurrent import/upsert of the same phone/email — the row IS
			// represented, just by the winner's contact, not a failure to report.
			if existing, ferr := s.findExistingContact(ctx, tenantID, emailNorm, phoneDigits); ferr == nil && existing != nil {
				return existing.ID, "existing", nil
			}
			// Otherwise it's our own requested id colliding — surface a clear error rather than
			// the raw constraint message.
			return uuid.Nil, "failed", fmt.Errorf("create contact: id or unique field conflict: %w", err)
		}
		return uuid.Nil, "failed", fmt.Errorf("create contact: %w", err)
	}
	return created.ID, "created", nil
}
