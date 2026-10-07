package campaigns

import (
	"context"
	"fmt"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/campaign"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Service handles campaign business logic.
type Service struct {
	client      *ent.Client
	logger      *zap.Logger
	broadcaster Broadcaster
}

// NewService creates a new campaign service.
func NewService(client *ent.Client, logger *zap.Logger) *Service {
	return &Service{
		client: client,
		logger: logger.Named("campaigns"),
	}
}

// CreateInput contains the data needed to create a campaign.
type CreateInput struct {
	TenantID       uuid.UUID
	Name           string
	CampaignType   *string
	Platform       *string
	MetaCampaignID *string
	MetaAdsetID    *string
	MetaAdID       *string
	BudgetDaily    *float64
	BudgetTotal    *float64
	Currency       *string
	TargetAudience map[string]any
	FunnelID       *uuid.UUID
	StartDate      *time.Time
	EndDate        *time.Time
	Metrics        map[string]any
	Metadata       map[string]any
}

// UpdateInput contains the data to update a campaign.
type UpdateInput struct {
	Name           *string
	CampaignType   *string
	Platform       *string
	Status         *string
	MetaCampaignID *string
	MetaAdsetID    *string
	MetaAdID       *string
	BudgetDaily    *float64
	BudgetTotal    *float64
	Currency       *string
	TargetAudience map[string]any
	FunnelID       *uuid.UUID
	StartDate      *time.Time
	EndDate        *time.Time
	Metrics        map[string]any
	Metadata       map[string]any
}

// ListInput contains filtering/pagination for listing campaigns.
type ListInput struct {
	TenantID     uuid.UUID
	Status       *string
	CampaignType *string
	Platform     *string
	Limit        int
	Offset       int
}

// Create creates a new campaign.
func (s *Service) Create(ctx context.Context, in CreateInput) (*ent.Campaign, error) {
	builder := s.client.Campaign.Create().
		SetTenantID(in.TenantID).
		SetName(in.Name)

	if in.CampaignType != nil {
		builder.SetNillableCampaignType(in.CampaignType)
	}
	if in.Platform != nil {
		builder.SetNillablePlatform(in.Platform)
	}
	if in.MetaCampaignID != nil {
		builder.SetNillableMetaCampaignID(in.MetaCampaignID)
	}
	if in.MetaAdsetID != nil {
		builder.SetNillableMetaAdsetID(in.MetaAdsetID)
	}
	if in.MetaAdID != nil {
		builder.SetNillableMetaAdID(in.MetaAdID)
	}
	if in.BudgetDaily != nil {
		builder.SetNillableBudgetDaily(in.BudgetDaily)
	}
	if in.BudgetTotal != nil {
		builder.SetNillableBudgetTotal(in.BudgetTotal)
	}
	if in.Currency != nil {
		builder.SetNillableCurrency(in.Currency)
	}
	if in.TargetAudience != nil {
		builder.SetTargetAudience(in.TargetAudience)
	}
	if in.FunnelID != nil {
		builder.SetNillableFunnelID(in.FunnelID)
	}
	if in.StartDate != nil {
		builder.SetNillableStartDate(in.StartDate)
	}
	if in.EndDate != nil {
		builder.SetNillableEndDate(in.EndDate)
	}
	if in.Metrics != nil {
		builder.SetMetrics(in.Metrics)
	}
	if in.Metadata != nil {
		builder.SetMetadata(in.Metadata)
	}

	result, err := builder.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("create campaign: %w", err)
	}

	s.logger.Info("campaign created",
		zap.String("id", result.ID.String()),
		zap.String("tenant_id", in.TenantID.String()),
		zap.String("name", in.Name),
	)

	return result, nil
}

// Get retrieves a campaign by ID within a tenant.
func (s *Service) Get(ctx context.Context, tenantID, campaignID uuid.UUID) (*ent.Campaign, error) {
	result, err := s.client.Campaign.Query().
		Where(
			campaign.ID(campaignID),
			campaign.TenantID(tenantID),
		).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("get campaign: %w", err)
	}
	return result, nil
}

// List retrieves campaigns with filtering and pagination.
func (s *Service) List(ctx context.Context, in ListInput) ([]*ent.Campaign, int, error) {
	query := s.client.Campaign.Query().
		Where(campaign.TenantID(in.TenantID))

	if in.Status != nil {
		query = query.Where(campaign.StatusEQ(*in.Status))
	}
	if in.CampaignType != nil {
		query = query.Where(campaign.CampaignTypeEQ(*in.CampaignType))
	}
	if in.Platform != nil {
		query = query.Where(campaign.PlatformEQ(*in.Platform))
	}

	total, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("count campaigns: %w", err)
	}

	if in.Limit <= 0 {
		in.Limit = 20
	}
	if in.Limit > 100 {
		in.Limit = 100
	}

	results, err := query.
		Order(ent.Desc(campaign.FieldCreatedAt)).
		Limit(in.Limit).
		Offset(in.Offset).
		All(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("list campaigns: %w", err)
	}

	return results, total, nil
}

// Update updates a campaign.
func (s *Service) Update(ctx context.Context, tenantID, campaignID uuid.UUID, in UpdateInput) (*ent.Campaign, error) {
	existing, err := s.Get(ctx, tenantID, campaignID)
	if err != nil {
		return nil, err
	}

	builder := existing.Update()

	if in.Name != nil {
		builder.SetNillableName(in.Name)
	}
	if in.CampaignType != nil {
		builder.SetNillableCampaignType(in.CampaignType)
	}
	if in.Platform != nil {
		builder.SetNillablePlatform(in.Platform)
	}
	if in.Status != nil {
		builder.SetNillableStatus(in.Status)
	}
	if in.MetaCampaignID != nil {
		builder.SetNillableMetaCampaignID(in.MetaCampaignID)
	}
	if in.MetaAdsetID != nil {
		builder.SetNillableMetaAdsetID(in.MetaAdsetID)
	}
	if in.MetaAdID != nil {
		builder.SetNillableMetaAdID(in.MetaAdID)
	}
	if in.BudgetDaily != nil {
		builder.SetNillableBudgetDaily(in.BudgetDaily)
	}
	if in.BudgetTotal != nil {
		builder.SetNillableBudgetTotal(in.BudgetTotal)
	}
	if in.Currency != nil {
		builder.SetNillableCurrency(in.Currency)
	}
	if in.TargetAudience != nil {
		builder.SetTargetAudience(in.TargetAudience)
	}
	if in.FunnelID != nil {
		builder.SetNillableFunnelID(in.FunnelID)
	}
	if in.StartDate != nil {
		builder.SetNillableStartDate(in.StartDate)
	}
	if in.EndDate != nil {
		builder.SetNillableEndDate(in.EndDate)
	}
	if in.Metrics != nil {
		builder.SetMetrics(in.Metrics)
	}
	if in.Metadata != nil {
		builder.SetMetadata(in.Metadata)
	}

	result, err := builder.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("update campaign: %w", err)
	}

	return result, nil
}

// Delete soft-deletes a campaign by marking it as completed.
func (s *Service) Delete(ctx context.Context, tenantID, campaignID uuid.UUID) error {
	_, err := s.client.Campaign.Update().
		Where(
			campaign.ID(campaignID),
			campaign.TenantID(tenantID),
		).
		SetStatus("completed").
		Save(ctx)
	if err != nil {
		return fmt.Errorf("delete campaign: %w", err)
	}
	return nil
}

// Pause pauses a campaign by setting status to paused.
func (s *Service) Pause(ctx context.Context, tenantID, campaignID uuid.UUID) (*ent.Campaign, error) {
	existing, err := s.Get(ctx, tenantID, campaignID)
	if err != nil {
		return nil, err
	}

	result, err := existing.Update().
		SetStatus("paused").
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("pause campaign: %w", err)
	}

	s.logger.Info("campaign paused",
		zap.String("id", campaignID.String()),
		zap.String("tenant_id", tenantID.String()),
	)

	return result, nil
}

// Resume resumes a campaign by setting status to active.
func (s *Service) Resume(ctx context.Context, tenantID, campaignID uuid.UUID) (*ent.Campaign, error) {
	existing, err := s.Get(ctx, tenantID, campaignID)
	if err != nil {
		return nil, err
	}

	result, err := existing.Update().
		SetStatus("active").
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("resume campaign: %w", err)
	}

	s.logger.Info("campaign resumed",
		zap.String("id", campaignID.String()),
		zap.String("tenant_id", tenantID.String()),
	)

	return result, nil
}
