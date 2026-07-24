package contacts

import (
	"context"
	"fmt"
	"strings"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/chatsession"
	"github.com/bengobox/marketflow-api/internal/ent/contact"
	"github.com/bengobox/marketflow-api/internal/ent/deal"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// nationalSubscriberDigits strips non-digits and returns the last 9 digits — the Kenyan national
// subscriber number, format-agnostic ("0712345678", "254712345678" and "+254 712 345 678" all
// reduce to "712345678"). Mirrors the exact convention pos-api already uses for loyalty phone
// matching (internal/http/handlers/loyalty.go) — reused here rather than reinvented so "the same
// phone" means the same thing fleet-wide.
func nationalSubscriberDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteByte(byte(r))
		}
	}
	d := b.String()
	if len(d) > 9 {
		return d[len(d)-9:]
	}
	return d
}

// Service handles contact business logic.
type Service struct {
	client *ent.Client
	logger *zap.Logger
}

// NewService creates a new contact service.
func NewService(client *ent.Client, logger *zap.Logger) *Service {
	return &Service{
		client: client,
		logger: logger.Named("contacts"),
	}
}

// CreateInput contains the data needed to create a contact.
type CreateInput struct {
	TenantID         uuid.UUID
	Email            *string
	Phone            *string
	FirstName        *string
	LastName         *string
	ContactType      string
	LeadID           *uuid.UUID
	FLPCustomerID    *string
	ProductInterests []string
	Metadata         map[string]any
}

// UpdateInput contains the data to update a contact.
type UpdateInput struct {
	Email            *string
	Phone            *string
	FirstName        *string
	LastName         *string
	ContactType      *string
	FLPCustomerID    *string
	TotalCC          *float64
	TotalOrders      *int
	ProductInterests []string
	SubscribedEmail  *bool
	SubscribedSMS    *bool
	Metadata         map[string]any
}

// ListInput contains filtering/pagination for listing contacts.
type ListInput struct {
	TenantID    uuid.UUID
	ContactType *string
	Query       *string // optional name/email substring search
	Limit       int
	Offset      int
}

// Create creates a new contact with deduplication by email/phone. Dedup is format-agnostic: email
// compares case-insensitively (trimmed+lowercased) and phone compares by national subscriber
// digits (last 9) — a customer entered once as "0712345678" and again as "+254 712 345 678" or
// "254712345678" is the SAME contact, not three. Previously this compared raw, unnormalized
// strings, so any formatting difference across callers (POS till entry vs. checkout autofill vs.
// treasury's Add-Client modal) silently created a new Contact row per format — which then split
// that customer's downstream AR/loyalty history across multiple linked ledgers (see
// arpa.ReconcileSplitCustomerBalances, which explicitly cannot heal a split caused upstream here:
// it refuses to merge across >1 distinct crm_contact_id). The ORIGINAL stored value's format is
// left untouched (never rewritten) — only the comparison is normalized — so this never disturbs
// whatever dialable/display format a phone was first saved in.
func (s *Service) Create(ctx context.Context, in CreateInput) (*ent.Contact, error) {
	// Dedup: check if a contact with the same email or phone already exists for this tenant
	if in.Email != nil {
		if email := strings.ToLower(strings.TrimSpace(*in.Email)); email != "" {
			existing, err := s.client.Contact.Query().
				Where(
					contact.TenantID(in.TenantID),
					contact.EmailEqualFold(email),
				).
				First(ctx)
			if err == nil {
				s.logger.Info("contact already exists (by email), returning existing",
					zap.String("id", existing.ID.String()))
				return existing, nil
			}
		}
	}
	if in.Phone != nil {
		if digits := nationalSubscriberDigits(*in.Phone); digits != "" {
			existing, err := s.client.Contact.Query().
				Where(
					contact.TenantID(in.TenantID),
					contact.PhoneHasSuffix(digits),
				).
				First(ctx)
			if err == nil {
				s.logger.Info("contact already exists (by phone), returning existing",
					zap.String("id", existing.ID.String()))
				return existing, nil
			}
		}
	}

	builder := s.client.Contact.Create().
		SetTenantID(in.TenantID).
		SetContactType(in.ContactType)

	if in.Email != nil {
		builder.SetNillableEmail(in.Email)
	}
	if in.Phone != nil {
		builder.SetNillablePhone(in.Phone)
	}
	if in.FirstName != nil {
		builder.SetNillableFirstName(in.FirstName)
	}
	if in.LastName != nil {
		builder.SetNillableLastName(in.LastName)
	}
	if in.LeadID != nil {
		builder.SetNillableLeadID(in.LeadID)
	}
	if in.FLPCustomerID != nil {
		builder.SetNillableFlpCustomerID(in.FLPCustomerID)
	}
	if in.ProductInterests != nil {
		builder.SetProductInterests(in.ProductInterests)
	}
	if in.Metadata != nil {
		builder.SetMetadata(in.Metadata)
	}

	result, err := builder.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("create contact: %w", err)
	}

	s.logger.Info("contact created",
		zap.String("id", result.ID.String()),
		zap.String("tenant_id", in.TenantID.String()),
	)

	return result, nil
}

// Get retrieves a contact by ID within a tenant.
func (s *Service) Get(ctx context.Context, tenantID, contactID uuid.UUID) (*ent.Contact, error) {
	result, err := s.client.Contact.Query().
		Where(
			contact.ID(contactID),
			contact.TenantID(tenantID),
		).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("get contact: %w", err)
	}
	return result, nil
}

// List retrieves contacts with filtering and pagination.
func (s *Service) List(ctx context.Context, in ListInput) ([]*ent.Contact, int, error) {
	query := s.client.Contact.Query().
		Where(contact.TenantID(in.TenantID))

	if in.ContactType != nil {
		query = query.Where(contact.ContactTypeEQ(*in.ContactType))
	}
	if in.Query != nil && *in.Query != "" {
		q := *in.Query
		query = query.Where(contact.Or(
			contact.FirstNameContainsFold(q),
			contact.LastNameContainsFold(q),
			contact.EmailContainsFold(q),
		))
	}

	total, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("count contacts: %w", err)
	}

	if in.Limit <= 0 {
		in.Limit = 20
	}
	if in.Limit > 100 {
		in.Limit = 100
	}

	results, err := query.
		Order(ent.Desc(contact.FieldCreatedAt)).
		Limit(in.Limit).
		Offset(in.Offset).
		All(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("list contacts: %w", err)
	}

	return results, total, nil
}

// Update updates a contact.
func (s *Service) Update(ctx context.Context, tenantID, contactID uuid.UUID, in UpdateInput) (*ent.Contact, error) {
	existing, err := s.Get(ctx, tenantID, contactID)
	if err != nil {
		return nil, err
	}

	builder := existing.Update()

	if in.Email != nil {
		builder.SetNillableEmail(in.Email)
	}
	if in.Phone != nil {
		builder.SetNillablePhone(in.Phone)
	}
	if in.FirstName != nil {
		builder.SetNillableFirstName(in.FirstName)
	}
	if in.LastName != nil {
		builder.SetNillableLastName(in.LastName)
	}
	if in.ContactType != nil {
		builder.SetContactType(*in.ContactType)
	}
	if in.FLPCustomerID != nil {
		builder.SetNillableFlpCustomerID(in.FLPCustomerID)
	}
	if in.TotalCC != nil {
		builder.SetTotalCc(*in.TotalCC)
	}
	if in.TotalOrders != nil {
		builder.SetTotalOrders(*in.TotalOrders)
	}
	if in.ProductInterests != nil {
		builder.SetProductInterests(in.ProductInterests)
	}
	if in.SubscribedEmail != nil {
		builder.SetSubscribedEmail(*in.SubscribedEmail)
	}
	if in.SubscribedSMS != nil {
		builder.SetSubscribedSms(*in.SubscribedSMS)
	}
	if in.Metadata != nil {
		builder.SetMetadata(in.Metadata)
	}

	result, err := builder.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("update contact: %w", err)
	}

	return result, nil
}

// MergeResult reports what Merge repointed.
type MergeResult struct {
	SurvivorID uuid.UUID   `json:"survivor_id"`
	MergedIDs  []uuid.UUID `json:"merged_ids"`
	DealsMoved int         `json:"deals_moved"`
	ChatsMoved int         `json:"chats_moved"`
}

// Merge folds one or more duplicate contacts into a single survivor — the fix for contacts that
// were already split by the exact-string phone/email dedup bug (see Create's normalization
// above): repoints marketflow's own FKs (deals, chat_sessions), tags each duplicate's metadata so
// it's never mistaken for a live contact again (soft-marked, not hard-deleted — same audit-trail
// pattern used elsewhere in this codebase for merged ingredient/customer records), then publishes
// crm.contact.merged so treasury/pos (which each keep their OWN crm_contact_id-bearing rows, per
// feedback_service_data_ownership) can repoint and fold their own split records — e.g. treasury's
// customer_balances, which otherwise stays split forever (its own reconcile tool explicitly
// refuses to merge across >1 distinct crm_contact_id, by design).
func (s *Service) Merge(ctx context.Context, tenantID, survivorID uuid.UUID, duplicateIDs []uuid.UUID) (*MergeResult, error) {
	merged := make([]uuid.UUID, 0, len(duplicateIDs))
	for _, id := range duplicateIDs {
		if id != survivorID {
			merged = append(merged, id)
		}
	}
	if len(merged) == 0 {
		return nil, fmt.Errorf("no distinct duplicate contact ids to merge")
	}

	survivor, err := s.client.Contact.Query().
		Where(contact.ID(survivorID), contact.TenantID(tenantID)).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("load survivor contact: %w", err)
	}
	if into, ok := survivor.Metadata["merged_into"]; ok {
		return nil, fmt.Errorf("survivor contact was itself already merged into %v — merge into that contact instead", into)
	}
	dups, err := s.client.Contact.Query().
		Where(contact.IDIn(merged...), contact.TenantID(tenantID)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("load duplicate contacts: %w", err)
	}
	if len(dups) != len(merged) {
		return nil, fmt.Errorf("one or more duplicate contact ids do not belong to this tenant")
	}

	dealsMoved, err := s.client.Deal.Update().
		Where(deal.ContactIDIn(merged...)).
		SetContactID(survivorID).
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("repoint deals: %w", err)
	}
	chatsMoved, err := s.client.ChatSession.Update().
		Where(chatsession.ContactIDIn(merged...)).
		SetContactID(survivorID).
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("repoint chat sessions: %w", err)
	}

	for _, d := range dups {
		meta := d.Metadata
		if meta == nil {
			meta = map[string]any{}
		}
		meta["merged_into"] = survivorID.String()
		if _, err := s.client.Contact.UpdateOneID(d.ID).SetMetadata(meta).Save(ctx); err != nil {
			s.logger.Warn("contact merge: failed to tag merged-into metadata", zap.String("contact_id", d.ID.String()), zap.Error(err))
		}
	}

	event := eventslib.NewEvent("contact.merged", "crm", survivorID, tenantID, map[string]any{
		"survivor_id": survivorID.String(),
		"merged_ids":  uuidsToStrings(merged),
	})
	payload, jerr := event.ToJSON()
	if jerr != nil {
		s.logger.Warn("contact merge: failed to marshal crm.contact.merged event", zap.Error(jerr))
	} else if _, cerr := s.client.OutboxEvent.Create().
		SetID(event.ID).
		SetTenantID(tenantID).
		SetAggregateType(event.AggregateType).
		SetAggregateID(event.AggregateID).
		SetEventType(event.EventType).
		SetPayload(payload).
		SetStatus("PENDING").
		Save(ctx); cerr != nil {
		// Non-fatal: the FK repoint + metadata tagging above already succeeded (the marketflow
		// side of the merge is complete); a failed event just means treasury/pos won't
		// auto-repoint their own split records until this is retried.
		s.logger.Error("contact merge: failed to enqueue crm.contact.merged outbox event", zap.Error(cerr))
	}

	s.logger.Info("contacts merged",
		zap.String("survivor_id", survivorID.String()),
		zap.Int("merged_count", len(merged)),
		zap.Int("deals_moved", dealsMoved),
		zap.Int("chats_moved", chatsMoved),
	)
	return &MergeResult{SurvivorID: survivorID, MergedIDs: merged, DealsMoved: dealsMoved, ChatsMoved: chatsMoved}, nil
}

func uuidsToStrings(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

// Delete removes a contact.
func (s *Service) Delete(ctx context.Context, tenantID, contactID uuid.UUID) error {
	_, err := s.client.Contact.Delete().
		Where(
			contact.ID(contactID),
			contact.TenantID(tenantID),
		).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("delete contact: %w", err)
	}
	return nil
}
