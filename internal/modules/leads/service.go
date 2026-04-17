package leads

import (
	"context"
	"fmt"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Service handles lead business logic.
type Service struct {
	client *ent.Client
	logger *zap.Logger
}

// NewService creates a new lead service.
func NewService(client *ent.Client, logger *zap.Logger) *Service {
	return &Service{
		client: client,
		logger: logger.Named("leads"),
	}
}

// CreateInput contains the data needed to create a lead.
type CreateInput struct {
	TenantID             uuid.UUID
	Email                *string
	Phone                *string
	FirstName            *string
	LastName             *string
	Source               string
	CampaignID           *uuid.UUID
	FunnelID             *uuid.UUID
	UTMSource            *string
	UTMMedium            *string
	UTMCampaign          *string
	LeadType             string
	Interest             *string
	QualificationAnswers map[string]any
	Metadata             map[string]any
}

// UpdateInput contains the data to update a lead.
type UpdateInput struct {
	Email                *string
	Phone                *string
	FirstName            *string
	LastName             *string
	Status               *string
	Score                *int
	Interest             *string
	QualificationAnswers map[string]any
	Metadata             map[string]any
}

// ListInput contains filtering/pagination for listing leads.
type ListInput struct {
	TenantID uuid.UUID
	Status   *string
	Source   *string
	LeadType *string
	From     *time.Time
	To       *time.Time
	Limit    int
	Offset   int
}

// Create creates a new lead.
func (s *Service) Create(ctx context.Context, in CreateInput) (*ent.Lead, error) {
	builder := s.client.Lead.Create().
		SetTenantID(in.TenantID).
		SetSource(in.Source).
		SetLeadType(in.LeadType)

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
	if in.CampaignID != nil {
		builder.SetNillableCampaignID(in.CampaignID)
	}
	if in.FunnelID != nil {
		builder.SetNillableFunnelID(in.FunnelID)
	}
	if in.UTMSource != nil {
		builder.SetNillableUtmSource(in.UTMSource)
	}
	if in.UTMMedium != nil {
		builder.SetNillableUtmMedium(in.UTMMedium)
	}
	if in.UTMCampaign != nil {
		builder.SetNillableUtmCampaign(in.UTMCampaign)
	}
	if in.Interest != nil {
		builder.SetNillableInterest(in.Interest)
	}
	if in.QualificationAnswers != nil {
		builder.SetQualificationAnswers(in.QualificationAnswers)
	}
	if in.Metadata != nil {
		builder.SetMetadata(in.Metadata)
	}

	result, err := builder.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("create lead: %w", err)
	}

	s.logger.Info("lead created",
		zap.String("id", result.ID.String()),
		zap.String("tenant_id", in.TenantID.String()),
		zap.String("source", in.Source),
	)

	return result, nil
}

// Get retrieves a lead by ID within a tenant.
func (s *Service) Get(ctx context.Context, tenantID, leadID uuid.UUID) (*ent.Lead, error) {
	result, err := s.client.Lead.Query().
		Where(
			lead.ID(leadID),
			lead.TenantID(tenantID),
		).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("get lead: %w", err)
	}
	return result, nil
}

// List retrieves leads with filtering and pagination.
func (s *Service) List(ctx context.Context, in ListInput) ([]*ent.Lead, int, error) {
	query := s.client.Lead.Query().
		Where(lead.TenantID(in.TenantID))

	if in.Status != nil {
		query = query.Where(lead.StatusEQ(*in.Status))
	}
	if in.Source != nil {
		query = query.Where(lead.SourceEQ(*in.Source))
	}
	if in.LeadType != nil {
		query = query.Where(lead.LeadTypeEQ(*in.LeadType))
	}
	if in.From != nil {
		query = query.Where(lead.CreatedAtGTE(*in.From))
	}
	if in.To != nil {
		query = query.Where(lead.CreatedAtLTE(*in.To))
	}

	total, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("count leads: %w", err)
	}

	if in.Limit <= 0 {
		in.Limit = 20
	}
	if in.Limit > 100 {
		in.Limit = 100
	}

	results, err := query.
		Order(ent.Desc(lead.FieldCreatedAt)).
		Limit(in.Limit).
		Offset(in.Offset).
		All(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("list leads: %w", err)
	}

	return results, total, nil
}

// ListAll retrieves all leads matching the filter (no pagination). Used for CSV export.
func (s *Service) ListAll(ctx context.Context, in ListInput) ([]*ent.Lead, error) {
	query := s.client.Lead.Query().
		Where(lead.TenantID(in.TenantID))

	if in.Status != nil {
		query = query.Where(lead.StatusEQ(*in.Status))
	}
	if in.Source != nil {
		query = query.Where(lead.SourceEQ(*in.Source))
	}
	if in.LeadType != nil {
		query = query.Where(lead.LeadTypeEQ(*in.LeadType))
	}
	if in.From != nil {
		query = query.Where(lead.CreatedAtGTE(*in.From))
	}
	if in.To != nil {
		query = query.Where(lead.CreatedAtLTE(*in.To))
	}

	results, err := query.
		Order(ent.Desc(lead.FieldCreatedAt)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("list all leads: %w", err)
	}

	return results, nil
}

// Update updates a lead.
func (s *Service) Update(ctx context.Context, tenantID, leadID uuid.UUID, in UpdateInput) (*ent.Lead, error) {
	existing, err := s.Get(ctx, tenantID, leadID)
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
	if in.Score != nil {
		builder.SetScore(*in.Score)
	}
	if in.Interest != nil {
		builder.SetNillableInterest(in.Interest)
	}
	if in.QualificationAnswers != nil {
		builder.SetQualificationAnswers(in.QualificationAnswers)
	}
	if in.Metadata != nil {
		builder.SetMetadata(in.Metadata)
	}

	// Handle status transitions with timestamp tracking
	if in.Status != nil && *in.Status != existing.Status {
		builder.SetStatus(*in.Status)
		now := time.Now()
		switch *in.Status {
		case "contacted":
			builder.SetContactedAt(now)
		case "qualified":
			builder.SetQualifiedAt(now)
		case "converted":
			builder.SetConvertedAt(now)
		}
	}

	result, err := builder.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("update lead: %w", err)
	}

	return result, nil
}

// Delete soft-deletes a lead by marking it as lost.
func (s *Service) Delete(ctx context.Context, tenantID, leadID uuid.UUID) error {
	_, err := s.client.Lead.Update().
		Where(
			lead.ID(leadID),
			lead.TenantID(tenantID),
		).
		SetStatus("lost").
		Save(ctx)
	if err != nil {
		return fmt.Errorf("delete lead: %w", err)
	}
	return nil
}
