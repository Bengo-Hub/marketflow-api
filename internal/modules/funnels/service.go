package funnels

import (
	"context"
	"fmt"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/funnel"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Service handles funnel business logic.
type Service struct {
	client *ent.Client
	logger *zap.Logger
}

// NewService creates a new funnel service.
func NewService(client *ent.Client, logger *zap.Logger) *Service {
	return &Service{
		client: client,
		logger: logger.Named("funnels"),
	}
}

// CreateInput contains the data needed to create a funnel.
type CreateInput struct {
	TenantID       uuid.UUID
	Name           string
	Slug           string
	FunnelType     *string
	Steps          []map[string]any
	LandingPageHTML *string
	LandingPageCSS map[string]any
	Metadata       map[string]any
}

// UpdateInput contains the data to update a funnel.
type UpdateInput struct {
	Name            *string
	Slug            *string
	FunnelType      *string
	Status          *string
	Steps           []map[string]any
	LandingPageHTML *string
	LandingPageCSS  map[string]any
	Metadata        map[string]any
}

// ListInput contains filtering/pagination for listing funnels.
type ListInput struct {
	TenantID   uuid.UUID
	Status     *string
	FunnelType *string
	Limit      int
	Offset     int
}

// Create creates a new funnel.
func (s *Service) Create(ctx context.Context, in CreateInput) (*ent.Funnel, error) {
	builder := s.client.Funnel.Create().
		SetTenantID(in.TenantID).
		SetName(in.Name).
		SetSlug(in.Slug)

	if in.FunnelType != nil {
		builder.SetNillableFunnelType(in.FunnelType)
	}
	if in.Steps != nil {
		builder.SetSteps(in.Steps)
	}
	if in.LandingPageHTML != nil {
		builder.SetNillableLandingPageHTML(in.LandingPageHTML)
	}
	if in.LandingPageCSS != nil {
		builder.SetLandingPageCSS(in.LandingPageCSS)
	}
	if in.Metadata != nil {
		builder.SetMetadata(in.Metadata)
	}

	result, err := builder.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("create funnel: %w", err)
	}

	s.logger.Info("funnel created",
		zap.String("id", result.ID.String()),
		zap.String("tenant_id", in.TenantID.String()),
		zap.String("slug", in.Slug),
	)

	return result, nil
}

// Get retrieves a funnel by ID within a tenant.
func (s *Service) Get(ctx context.Context, tenantID, funnelID uuid.UUID) (*ent.Funnel, error) {
	result, err := s.client.Funnel.Query().
		Where(
			funnel.ID(funnelID),
			funnel.TenantID(tenantID),
		).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("get funnel: %w", err)
	}
	return result, nil
}

// GetBySlug retrieves a published funnel by slug (public endpoint, no tenant filter).
func (s *Service) GetBySlug(ctx context.Context, slug string) (*ent.Funnel, error) {
	result, err := s.client.Funnel.Query().
		Where(
			funnel.SlugEQ(slug),
			funnel.StatusEQ("published"),
		).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("get funnel by slug: %w", err)
	}
	return result, nil
}

// List retrieves funnels with filtering and pagination.
func (s *Service) List(ctx context.Context, in ListInput) ([]*ent.Funnel, int, error) {
	query := s.client.Funnel.Query().
		Where(funnel.TenantID(in.TenantID))

	if in.Status != nil {
		query = query.Where(funnel.StatusEQ(*in.Status))
	}
	if in.FunnelType != nil {
		query = query.Where(funnel.FunnelTypeEQ(*in.FunnelType))
	}

	total, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("count funnels: %w", err)
	}

	if in.Limit <= 0 {
		in.Limit = 20
	}
	if in.Limit > 100 {
		in.Limit = 100
	}

	results, err := query.
		Order(ent.Desc(funnel.FieldCreatedAt)).
		Limit(in.Limit).
		Offset(in.Offset).
		All(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("list funnels: %w", err)
	}

	return results, total, nil
}

// Update updates a funnel.
func (s *Service) Update(ctx context.Context, tenantID, funnelID uuid.UUID, in UpdateInput) (*ent.Funnel, error) {
	existing, err := s.Get(ctx, tenantID, funnelID)
	if err != nil {
		return nil, err
	}

	builder := existing.Update()

	if in.Name != nil {
		builder.SetName(*in.Name)
	}
	if in.Slug != nil {
		builder.SetSlug(*in.Slug)
	}
	if in.FunnelType != nil {
		builder.SetFunnelType(*in.FunnelType)
	}
	if in.Status != nil {
		builder.SetStatus(*in.Status)
	}
	if in.Steps != nil {
		builder.SetSteps(in.Steps)
	}
	if in.LandingPageHTML != nil {
		builder.SetNillableLandingPageHTML(in.LandingPageHTML)
	}
	if in.LandingPageCSS != nil {
		builder.SetLandingPageCSS(in.LandingPageCSS)
	}
	if in.Metadata != nil {
		builder.SetMetadata(in.Metadata)
	}

	result, err := builder.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("update funnel: %w", err)
	}

	return result, nil
}

// Delete soft-deletes a funnel by marking it as archived.
func (s *Service) Delete(ctx context.Context, tenantID, funnelID uuid.UUID) error {
	_, err := s.client.Funnel.Update().
		Where(
			funnel.ID(funnelID),
			funnel.TenantID(tenantID),
		).
		SetStatus("archived").
		Save(ctx)
	if err != nil {
		return fmt.Errorf("delete funnel: %w", err)
	}
	return nil
}

// Publish publishes a funnel by setting status to published and published_at to now.
func (s *Service) Publish(ctx context.Context, tenantID, funnelID uuid.UUID) (*ent.Funnel, error) {
	existing, err := s.Get(ctx, tenantID, funnelID)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	result, err := existing.Update().
		SetStatus("published").
		SetPublishedAt(now).
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("publish funnel: %w", err)
	}

	s.logger.Info("funnel published",
		zap.String("id", funnelID.String()),
		zap.String("tenant_id", tenantID.String()),
	)

	return result, nil
}
