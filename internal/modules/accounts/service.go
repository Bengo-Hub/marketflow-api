package accounts

import (
	"context"
	"fmt"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/account"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Service handles account (company) business logic.
type Service struct {
	client *ent.Client
	logger *zap.Logger
}

// NewService creates a new account service.
func NewService(client *ent.Client, logger *zap.Logger) *Service {
	return &Service{
		client: client,
		logger: logger.Named("accounts"),
	}
}

// CreateInput contains data needed to create an account.
type CreateInput struct {
	TenantID       uuid.UUID
	Name           string
	Industry       *string
	Website        *string
	Phone          *string
	Address        map[string]any
	EmployeeCount  *int
	AnnualRevenue  *float64
	OwnerUserID    *uuid.UUID
	Metadata       map[string]any
}

// UpdateInput contains data to update an account.
type UpdateInput struct {
	Name          *string
	Industry      *string
	Website       *string
	Phone         *string
	Address       map[string]any
	EmployeeCount *int
	AnnualRevenue *float64
	OwnerUserID   *uuid.UUID
	Metadata      map[string]any
}

// ListInput contains filtering and pagination for listing accounts.
type ListInput struct {
	TenantID uuid.UUID
	Industry *string
	OwnerID  *uuid.UUID
	Limit    int
	Offset   int
}

// Create creates a new account.
func (s *Service) Create(ctx context.Context, in CreateInput) (*ent.Account, error) {
	builder := s.client.Account.Create().
		SetTenantID(in.TenantID).
		SetName(in.Name)

	if in.Industry != nil {
		builder.SetNillableIndustry(in.Industry)
	}
	if in.Website != nil {
		builder.SetNillableWebsite(in.Website)
	}
	if in.Phone != nil {
		builder.SetNillablePhone(in.Phone)
	}
	if in.Address != nil {
		builder.SetAddress(in.Address)
	}
	if in.EmployeeCount != nil {
		builder.SetNillableEmployeeCount(in.EmployeeCount)
	}
	if in.AnnualRevenue != nil {
		builder.SetNillableAnnualRevenue(in.AnnualRevenue)
	}
	if in.OwnerUserID != nil {
		builder.SetNillableOwnerUserID(in.OwnerUserID)
	}
	if in.Metadata != nil {
		builder.SetMetadata(in.Metadata)
	}

	result, err := builder.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("create account: %w", err)
	}
	s.logger.Info("account created",
		zap.String("id", result.ID.String()),
		zap.String("name", in.Name),
	)
	return result, nil
}

// Get retrieves an account by ID within a tenant.
func (s *Service) Get(ctx context.Context, tenantID, accountID uuid.UUID) (*ent.Account, error) {
	result, err := s.client.Account.Query().
		Where(account.ID(accountID), account.TenantID(tenantID)).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("get account: %w", err)
	}
	return result, nil
}

// List retrieves accounts with filtering and pagination.
func (s *Service) List(ctx context.Context, in ListInput) ([]*ent.Account, int, error) {
	query := s.client.Account.Query().Where(account.TenantID(in.TenantID))

	if in.Industry != nil {
		query = query.Where(account.IndustryEQ(*in.Industry))
	}
	if in.OwnerID != nil {
		query = query.Where(account.OwnerUserID(*in.OwnerID))
	}

	total, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("count accounts: %w", err)
	}

	if in.Limit <= 0 {
		in.Limit = 20
	}
	if in.Limit > 100 {
		in.Limit = 100
	}

	results, err := query.
		Order(ent.Asc(account.FieldName)).
		Limit(in.Limit).
		Offset(in.Offset).
		All(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("list accounts: %w", err)
	}
	return results, total, nil
}

// Update updates an account.
func (s *Service) Update(ctx context.Context, tenantID, accountID uuid.UUID, in UpdateInput) (*ent.Account, error) {
	existing, err := s.Get(ctx, tenantID, accountID)
	if err != nil {
		return nil, err
	}
	builder := existing.Update()
	if in.Name != nil {
		builder.SetName(*in.Name)
	}
	if in.Industry != nil {
		builder.SetNillableIndustry(in.Industry)
	}
	if in.Website != nil {
		builder.SetNillableWebsite(in.Website)
	}
	if in.Phone != nil {
		builder.SetNillablePhone(in.Phone)
	}
	if in.Address != nil {
		builder.SetAddress(in.Address)
	}
	if in.EmployeeCount != nil {
		builder.SetNillableEmployeeCount(in.EmployeeCount)
	}
	if in.AnnualRevenue != nil {
		builder.SetNillableAnnualRevenue(in.AnnualRevenue)
	}
	if in.OwnerUserID != nil {
		builder.SetNillableOwnerUserID(in.OwnerUserID)
	}
	if in.Metadata != nil {
		builder.SetMetadata(in.Metadata)
	}
	result, err := builder.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("update account: %w", err)
	}
	return result, nil
}

// Delete hard-deletes an account.
func (s *Service) Delete(ctx context.Context, tenantID, accountID uuid.UUID) error {
	n, err := s.client.Account.Delete().
		Where(account.ID(accountID), account.TenantID(tenantID)).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("delete account: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("account not found")
	}
	return nil
}

// ListContacts retrieves contacts linked to an account.
func (s *Service) ListContacts(ctx context.Context, tenantID, accountID uuid.UUID) ([]*ent.Contact, error) {
	acc, err := s.client.Account.Query().
		Where(account.ID(accountID), account.TenantID(tenantID)).
		WithContacts().
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("get account contacts: %w", err)
	}
	return acc.Edges.Contacts, nil
}
