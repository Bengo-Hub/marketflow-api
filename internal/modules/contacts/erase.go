package contacts

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/activity"
	"github.com/bengobox/marketflow-api/internal/ent/chatsession"
	"github.com/bengobox/marketflow-api/internal/ent/contact"
	"github.com/bengobox/marketflow-api/internal/ent/customfieldvalue"
	"github.com/bengobox/marketflow-api/internal/ent/deal"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/ent/nurturedeliveryevent"
	"github.com/bengobox/marketflow-api/internal/ent/nurtureenrollment"
	"github.com/bengobox/marketflow-api/internal/ent/predicate"
	"github.com/bengobox/marketflow-api/internal/ent/scheduledmeeting"
	"github.com/bengobox/marketflow-api/internal/ent/task"
	"github.com/bengobox/marketflow-api/internal/outbox"
)

// ErasedName replaces the name of a contact (and its lead) stripped after the person asked to be
// erased, and the title of their tasks.
const ErasedName = "Erased"

// Audit events written by an erasure.
const (
	ContactErasedAudit        = "contact.erased"         // stripped to a minimal record
	ContactErasedDeletedAudit = "contact.erased_deleted" // deleted with everything about the person
	ContactErasureFailedAudit = "contact.erasure_failed" // the event could not be processed
)

// erasureKeptMetadata lists the metadata keys an erased contact keeps: codes, never personal data.
var erasureKeptMetadata = []string{"legacy_contact_id"}

// ErasureRecord says why a contact was stripped instead of deleted. It holds ids and short
// reasons only (for example "treasury: referenced"), never personal data.
type ErasureRecord struct {
	// Source is the event that asked for the erasure, e.g. privacy.party_anonymised.
	Source string
	// SourceID is the requesting service's own id for the person (maskani's party id).
	SourceID string
	Reasons  []string
}

func (rec ErasureRecord) auditPayload() map[string]any {
	return map[string]any{"source": rec.Source, "source_id": rec.SourceID, "reasons": rec.Reasons}
}

// Exists reports whether the tenant has a contact with this id.
func (s *Service) Exists(ctx context.Context, tenantID, contactID uuid.UUID) (bool, error) {
	ok, err := s.client.Contact.Query().Where(contact.ID(contactID), contact.TenantID(tenantID)).Exist(ctx)
	if err != nil {
		return false, fmt.Errorf("contact exists: %w", err)
	}
	return ok, nil
}

// Erase strips a contact to a minimal record when another service still links it, so it cannot
// be deleted. On the contact: email, phone, last name, the FLP id, enrichment data, interests and
// free-form metadata go, the name becomes "Erased", marketing subscriptions end, and the id stays
// so the linking services keep a valid reference. Everything else marketflow holds about the
// person is scrubbed in the same transaction (see scrubPersonRecords); ids and counts stay. The
// metadata and a tenant audit row record when and why; crm.contact.updated tells other services
// to refresh their snapshots.
//
// It is idempotent: a contact already erased changes nothing on the contact, records nothing and
// publishes nothing, and it answers false (the related records are scrubbed again, which is a
// no-op unless something new was added). Another tenant's id, or one already deleted, is a no-op.
func (s *Service) Erase(ctx context.Context, tenantID, contactID uuid.UUID, rec ErasureRecord) (bool, error) {
	var changed bool
	err := s.inTx(ctx, func(tx *ent.Tx) error {
		var err error
		changed, err = s.erase(ctx, tx, tenantID, contactID, rec)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("erase contact: %w", err)
	}
	return changed, nil
}

// DeleteErased deletes an erased person's contact when no other service links it, together with
// what marketflow holds about the person: activities and tasks on the contact, its lead and their
// personal deals, custom field values on the contact and lead, chat sessions, scheduled meetings and
// nurture enrollments (with their delivery events). The lead (campaign counts) and the personal
// deals (pipeline reports) stay, scrubbed of personal data. Company deals keep everything and only
// lose the person's link. It
// publishes crm.contact.deleted like the normal delete and writes a tenant audit row. It answers
// false when the contact is already gone (another tenant's id included).
func (s *Service) DeleteErased(ctx context.Context, tenantID, contactID uuid.UUID, rec ErasureRecord) (bool, error) {
	var deleted bool
	err := s.inTx(ctx, func(tx *ent.Tx) error {
		c, err := tx.Contact.Query().Where(contact.ID(contactID), contact.TenantID(tenantID)).Only(ctx)
		if ent.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		scope, err := loadPersonScope(ctx, tx, tenantID, contactID, c.LeadID)
		if err != nil {
			return err
		}
		if err := deletePersonRecords(ctx, tx, scope); err != nil {
			return err
		}
		if err := scrubLead(ctx, tx, scope); err != nil {
			return err
		}
		if err := scrubDeals(ctx, tx, scope); err != nil {
			return err
		}
		if err := unlinkCompanyDeals(ctx, tx, scope); err != nil {
			return err
		}
		if err := tx.Contact.DeleteOneID(contactID).Exec(ctx); err != nil {
			return err
		}
		if err := writeAudit(ctx, tx.AuditLog, tenantID, contactID, ContactErasedDeletedAudit, rec.auditPayload()); err != nil {
			return err
		}
		deleted = true
		return outbox.Enqueue(ctx, tx.OutboxEvent, contactDeletedEvent(contactID, tenantID))
	})
	if err != nil {
		return false, fmt.Errorf("delete erased contact: %w", err)
	}
	return deleted, nil
}

// RecordErasureFailure keeps a tenant audit row when an erasure event was given up on, so the
// request is not lost with a log line. reason is a short fixed text, never an error message
// that could carry personal data.
func (s *Service) RecordErasureFailure(ctx context.Context, tenantID, contactID uuid.UUID, rec ErasureRecord, reason string) error {
	p := rec.auditPayload()
	p["failure"] = reason
	return writeAudit(ctx, s.client.AuditLog, tenantID, contactID, ContactErasureFailedAudit, p)
}

func (s *Service) inTx(ctx context.Context, fn func(tx *ent.Tx) error) error {
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// writeAudit keeps a tenant audit row about the contact; ac is client.AuditLog or tx.AuditLog.
func writeAudit(ctx context.Context, ac *ent.AuditLogClient, tenantID, contactID uuid.UUID, event string, payload map[string]any) error {
	_, err := ac.Create().
		SetScope("tenant").SetTenantID(tenantID).SetEvent(event).
		SetActor("marketflow-worker").SetSubjectType("contact").SetSubjectID(contactID).
		SetPayload(payload).
		Save(ctx)
	return err
}

func (s *Service) erase(ctx context.Context, tx *ent.Tx, tenantID, contactID uuid.UUID, rec ErasureRecord) (bool, error) {
	existing, err := tx.Contact.Query().Where(contact.ID(contactID), contact.TenantID(tenantID)).Only(ctx)
	if ent.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	scope, err := loadPersonScope(ctx, tx, tenantID, contactID, existing.LeadID)
	if err != nil {
		return false, err
	}
	if err := scrubPersonRecords(ctx, tx, scope); err != nil {
		return false, err
	}
	changedFields := erasedIdentityFields(existing)
	if _, done := existing.Metadata["erased_at"]; done && len(changedFields) == 0 && !hasPersonalExtras(existing) {
		return false, nil
	}

	now := time.Now().UTC()
	meta := map[string]any{}
	for _, k := range erasureKeptMetadata {
		if v, ok := existing.Metadata[k]; ok {
			meta[k] = v
		}
	}
	meta["erased_at"] = now.Format(time.RFC3339)
	meta["erasure_source"] = rec.Source
	if rec.SourceID != "" {
		meta["erasure_source_id"] = rec.SourceID
	}
	meta["erasure_reasons"] = rec.Reasons

	if err := tx.Contact.UpdateOne(existing).
		ClearEmail().ClearEmailNormalized().
		ClearPhone().ClearPhoneSubscriberDigits().
		SetFirstName(ErasedName).ClearLastName().
		ClearFlpCustomerID().ClearEnrichmentData().ClearProductInterests().
		SetSubscribedEmail(false).SetSubscribedSms(false).
		SetMetadata(meta).
		Exec(ctx); err != nil {
		return false, err
	}
	if err := writeAudit(ctx, tx.AuditLog, tenantID, contactID, ContactErasedAudit, rec.auditPayload()); err != nil {
		return false, err
	}
	if len(changedFields) > 0 {
		if err := outbox.Enqueue(ctx, tx.OutboxEvent, contactUpdatedEvent(existing, changedFields)); err != nil {
			return false, err
		}
	}
	return true, nil
}

// personScope is one person in one tenant: their contact, the lead it was converted from (if any)
// and the deals linked to either. dealIDs are personal deals (no company account), scrubbed or
// cleared with the person; companyDealIDs belong to a company account and only lose the link.
type personScope struct {
	tenantID       uuid.UUID
	contactID      uuid.UUID
	leadID         *uuid.UUID
	dealIDs        []uuid.UUID
	companyDealIDs []uuid.UUID
}

// loadPersonScope finds the deals linked to the contact or its lead, in the tenant.
func loadPersonScope(ctx context.Context, tx *ent.Tx, tenantID, contactID uuid.UUID, leadID *uuid.UUID) (personScope, error) {
	p := personScope{tenantID: tenantID, contactID: contactID, leadID: leadID}
	or := []predicate.Deal{deal.ContactID(contactID)}
	if leadID != nil {
		or = append(or, deal.LeadID(*leadID))
	}
	deals, err := tx.Deal.Query().Where(deal.TenantID(tenantID), deal.Or(or...)).
		Select(deal.FieldID, deal.FieldAccountID).All(ctx)
	if err != nil {
		return p, fmt.Errorf("list deals: %w", err)
	}
	for _, d := range deals {
		if d.AccountID != nil {
			p.companyDealIDs = append(p.companyDealIDs, d.ID)
		} else {
			p.dealIDs = append(p.dealIDs, d.ID)
		}
	}
	return p, nil
}

// unlinkCompanyDeals clears only the erased person's link (contact and lead id) on deals that
// belong to a company account. Those deals, and their activities and tasks, hold other people's
// history, so they are neither renamed nor scrubbed nor deleted.
func unlinkCompanyDeals(ctx context.Context, tx *ent.Tx, p personScope) error {
	if len(p.companyDealIDs) == 0 {
		return nil
	}
	base := []predicate.Deal{deal.TenantID(p.tenantID), deal.IDIn(p.companyDealIDs...)}
	if _, err := tx.Deal.Update().Where(append(base, deal.ContactID(p.contactID))...).
		ClearContactID().Save(ctx); err != nil {
		return fmt.Errorf("unlink company deals: %w", err)
	}
	if p.leadID != nil {
		if _, err := tx.Deal.Update().Where(append(base, deal.LeadID(*p.leadID))...).
			ClearLeadID().Save(ctx); err != nil {
			return fmt.Errorf("unlink company deals: %w", err)
		}
	}
	return nil
}

// entityRef is one (entity_type, entity_id) pair that points at the person.
type entityRef struct {
	typ string
	id  uuid.UUID
}

// entityRefs lists the contact and lead, plus the person's deals when withDeals is set.
func (p personScope) entityRefs(withDeals bool) []entityRef {
	refs := []entityRef{{"contact", p.contactID}}
	if p.leadID != nil {
		refs = append(refs, entityRef{"lead", *p.leadID})
	}
	if withDeals {
		for _, id := range p.dealIDs {
			refs = append(refs, entityRef{"deal", id})
		}
	}
	return refs
}

// activities covers the contact, the lead and the person's deals.
func (p personScope) activities() predicate.Activity {
	var or []predicate.Activity
	for _, r := range p.entityRefs(true) {
		or = append(or, activity.And(activity.EntityType(r.typ), activity.EntityID(r.id)))
	}
	return activity.And(activity.TenantID(p.tenantID), activity.Or(or...))
}

// tasks covers the contact, the lead and the person's deals.
func (p personScope) tasks() predicate.Task {
	var or []predicate.Task
	for _, r := range p.entityRefs(true) {
		or = append(or, task.And(task.EntityType(r.typ), task.EntityID(r.id)))
	}
	return task.And(task.TenantID(p.tenantID), task.Or(or...))
}

// customFields covers the contact and the lead (a deal's custom fields are business data).
func (p personScope) customFields() predicate.CustomFieldValue {
	var or []predicate.CustomFieldValue
	for _, r := range p.entityRefs(false) {
		or = append(or, customfieldvalue.And(customfieldvalue.EntityType(r.typ), customfieldvalue.EntityID(r.id)))
	}
	return customfieldvalue.And(customfieldvalue.TenantID(p.tenantID), customfieldvalue.Or(or...))
}

// scrubDeals keeps the person's deals (value, stage and dates feed pipeline reports) but clears
// the title, lost reason and metadata, which may name the person.
func scrubDeals(ctx context.Context, tx *ent.Tx, p personScope) error {
	if len(p.dealIDs) == 0 {
		return nil
	}
	if _, err := tx.Deal.Update().Where(deal.TenantID(p.tenantID), deal.IDIn(p.dealIDs...)).
		SetTitle(ErasedName).ClearLostReason().ClearMetadata().Save(ctx); err != nil {
		return fmt.Errorf("scrub deals: %w", err)
	}
	return nil
}

func (p personScope) chats() predicate.ChatSession {
	or := []predicate.ChatSession{chatsession.ContactID(p.contactID)}
	if p.leadID != nil {
		or = append(or, chatsession.LeadID(*p.leadID))
	}
	return chatsession.And(chatsession.TenantID(p.tenantID), chatsession.Or(or...))
}

func (p personScope) meetings() predicate.ScheduledMeeting {
	or := []predicate.ScheduledMeeting{scheduledmeeting.ContactID(p.contactID)}
	if p.leadID != nil {
		or = append(or, scheduledmeeting.LeadID(*p.leadID))
	}
	return scheduledmeeting.And(scheduledmeeting.TenantID(p.tenantID), scheduledmeeting.Or(or...))
}

func (p personScope) enrollments() predicate.NurtureEnrollment {
	or := []predicate.NurtureEnrollment{nurtureenrollment.ContactID(p.contactID)}
	if p.leadID != nil {
		or = append(or, nurtureenrollment.LeadID(*p.leadID))
	}
	return nurtureenrollment.And(nurtureenrollment.TenantID(p.tenantID), nurtureenrollment.Or(or...))
}

// scrubPersonRecords clears personal data from everything marketflow holds about the person
// besides the contact row, keeping ids, types, timestamps and counts:
//   - the lead behind the contact: name, email, phone, interest, answers, scores, enrichment and
//     metadata (scrubLead);
//   - personal deals (no company account) linked to the contact or lead: title becomes "Erased",
//     lost reason and metadata go (value, stage and dates stay for pipeline reports); company
//     deals only lose the person's contact and lead link (unlinkCompanyDeals);
//   - activities on the contact, the lead or those personal deals: subject, body, outcome and metadata;
//   - tasks on them: title becomes "Erased", description goes;
//   - custom field values on the contact or lead: deleted (the value is all they hold);
//   - chat sessions: the transcript and metadata go, the counts stay;
//   - scheduled meetings: attendee name and email, notes and metadata go;
//   - nurture enrollments: running ones are cancelled with exit reason "erased"; metadata goes,
//     and so does the metadata of their delivery events.
func scrubPersonRecords(ctx context.Context, tx *ent.Tx, p personScope) error {
	if err := scrubLead(ctx, tx, p); err != nil {
		return err
	}
	if err := scrubDeals(ctx, tx, p); err != nil {
		return err
	}
	if err := unlinkCompanyDeals(ctx, tx, p); err != nil {
		return err
	}
	if _, err := tx.Activity.Update().Where(p.activities()).
		ClearSubject().ClearBody().ClearOutcome().ClearMetadata().Save(ctx); err != nil {
		return fmt.Errorf("scrub activities: %w", err)
	}
	if _, err := tx.Task.Update().Where(p.tasks()).
		SetTitle(ErasedName).ClearDescription().Save(ctx); err != nil {
		return fmt.Errorf("scrub tasks: %w", err)
	}
	if _, err := tx.CustomFieldValue.Delete().Where(p.customFields()).Exec(ctx); err != nil {
		return fmt.Errorf("delete custom field values: %w", err)
	}
	if _, err := tx.ChatSession.Update().Where(p.chats()).
		ClearMessages().ClearMetadata().Save(ctx); err != nil {
		return fmt.Errorf("scrub chat sessions: %w", err)
	}
	if _, err := tx.ScheduledMeeting.Update().Where(p.meetings()).
		ClearAttendeeName().ClearAttendeeEmail().ClearNotes().ClearMetadata().Save(ctx); err != nil {
		return fmt.Errorf("scrub meetings: %w", err)
	}
	now := time.Now().UTC()
	if _, err := tx.NurtureEnrollment.Update().
		Where(p.enrollments(), nurtureenrollment.StatusIn("active", "paused")).
		SetStatus("cancelled").SetExitReason("erased").SetCompletedAt(now).ClearNextStepAt().
		Save(ctx); err != nil {
		return fmt.Errorf("stop enrollments: %w", err)
	}
	enrollmentIDs, err := tx.NurtureEnrollment.Query().Where(p.enrollments()).IDs(ctx)
	if err != nil {
		return fmt.Errorf("list enrollments: %w", err)
	}
	if _, err := tx.NurtureEnrollment.Update().Where(p.enrollments()).ClearMetadata().Save(ctx); err != nil {
		return fmt.Errorf("scrub enrollments: %w", err)
	}
	if len(enrollmentIDs) > 0 {
		if _, err := tx.NurtureDeliveryEvent.Update().
			Where(nurturedeliveryevent.TenantID(p.tenantID), nurturedeliveryevent.EnrollmentIDIn(enrollmentIDs...)).
			ClearMetadata().Save(ctx); err != nil {
			return fmt.Errorf("scrub delivery events: %w", err)
		}
	}
	return nil
}

// deletePersonRecords removes the person's own records before the contact is deleted, so nothing
// about them survives as an unlinked row (the contact foreign keys are ON DELETE SET NULL).
func deletePersonRecords(ctx context.Context, tx *ent.Tx, p personScope) error {
	if _, err := tx.Activity.Delete().Where(p.activities()).Exec(ctx); err != nil {
		return fmt.Errorf("delete activities: %w", err)
	}
	if _, err := tx.Task.Delete().Where(p.tasks()).Exec(ctx); err != nil {
		return fmt.Errorf("delete tasks: %w", err)
	}
	if _, err := tx.CustomFieldValue.Delete().Where(p.customFields()).Exec(ctx); err != nil {
		return fmt.Errorf("delete custom field values: %w", err)
	}
	if _, err := tx.ChatSession.Delete().Where(p.chats()).Exec(ctx); err != nil {
		return fmt.Errorf("delete chat sessions: %w", err)
	}
	if _, err := tx.ScheduledMeeting.Delete().Where(p.meetings()).Exec(ctx); err != nil {
		return fmt.Errorf("delete meetings: %w", err)
	}
	enrollmentIDs, err := tx.NurtureEnrollment.Query().Where(p.enrollments()).IDs(ctx)
	if err != nil {
		return fmt.Errorf("list enrollments: %w", err)
	}
	if len(enrollmentIDs) > 0 {
		if _, err := tx.NurtureDeliveryEvent.Delete().
			Where(nurturedeliveryevent.TenantID(p.tenantID), nurturedeliveryevent.EnrollmentIDIn(enrollmentIDs...)).
			Exec(ctx); err != nil {
			return fmt.Errorf("delete delivery events: %w", err)
		}
		if _, err := tx.NurtureEnrollment.Delete().Where(nurtureenrollment.IDIn(enrollmentIDs...)).Exec(ctx); err != nil {
			return fmt.Errorf("delete enrollments: %w", err)
		}
	}
	return nil
}

// scrubLead clears the personal fields of the lead the contact was converted from. The lead row
// stays (campaign and funnel counts use it) with its source, status, score and timestamps.
func scrubLead(ctx context.Context, tx *ent.Tx, p personScope) error {
	if p.leadID == nil {
		return nil
	}
	_, err := tx.Lead.Update().Where(lead.ID(*p.leadID), lead.TenantID(p.tenantID)).
		ClearEmail().ClearPhone().SetFirstName(ErasedName).ClearLastName().
		ClearInterest().ClearQualificationAnswers().ClearAiScoreBreakdown().ClearEnrichmentData().
		SetMetadata(map[string]any{"erased_at": time.Now().UTC().Format(time.RFC3339)}).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("scrub lead: %w", err)
	}
	return nil
}

// erasedIdentityFields lists the identity fields erasing would change.
func erasedIdentityFields(c *ent.Contact) []string {
	var out []string
	if c.Email != nil && *c.Email != "" {
		out = append(out, "email")
	}
	if c.Phone != nil && *c.Phone != "" {
		out = append(out, "phone")
	}
	if c.FirstName == nil || *c.FirstName != ErasedName {
		out = append(out, "first_name")
	}
	if c.LastName != nil && *c.LastName != "" {
		out = append(out, "last_name")
	}
	return out
}

// hasPersonalExtras reports personal data outside the identity fields that erasing clears.
func hasPersonalExtras(c *ent.Contact) bool {
	if c.EmailNormalized != nil || c.PhoneSubscriberDigits != nil || c.FlpCustomerID != nil ||
		len(c.EnrichmentData) > 0 || len(c.ProductInterests) > 0 || c.SubscribedEmail || c.SubscribedSms {
		return true
	}
	for k := range c.Metadata {
		switch k {
		case "erased_at", "erasure_source", "erasure_source_id", "erasure_reasons":
			continue
		}
		if !slices.Contains(erasureKeptMetadata, k) {
			return true
		}
	}
	return false
}
