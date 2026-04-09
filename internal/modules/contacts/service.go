package contacts

import (
	"context"
	"fmt"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/contact"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

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
	Limit       int
	Offset      int
}

// Create creates a new contact with deduplication by email/phone.
func (s *Service) Create(ctx context.Context, in CreateInput) (*ent.Contact, error) {
	// Dedup: check if a contact with the same email or phone already exists for this tenant
	if in.Email != nil {
		existing, err := s.client.Contact.Query().
			Where(
				contact.TenantID(in.TenantID),
				contact.EmailEQ(*in.Email),
			).
			First(ctx)
		if err == nil {
			s.logger.Info("contact already exists (by email), returning existing",
				zap.String("id", existing.ID.String()))
			return existing, nil
		}
	}
	if in.Phone != nil {
		existing, err := s.client.Contact.Query().
			Where(
				contact.TenantID(in.TenantID),
				contact.PhoneEQ(*in.Phone),
			).
			First(ctx)
		if err == nil {
			s.logger.Info("contact already exists (by phone), returning existing",
				zap.String("id", existing.ID.String()))
			return existing, nil
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
