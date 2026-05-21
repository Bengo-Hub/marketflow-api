package pipelines

import (
	"context"
	"fmt"

	"github.com/bengobox/marketflow-api/internal/ent"
	entpipeline "github.com/bengobox/marketflow-api/internal/ent/pipeline"
	entpipelinestage "github.com/bengobox/marketflow-api/internal/ent/pipelinestage"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Service handles pipeline and stage business logic.
type Service struct {
	client *ent.Client
	logger *zap.Logger
}

// NewService creates a new pipeline service.
func NewService(client *ent.Client, logger *zap.Logger) *Service {
	return &Service{
		client: client,
		logger: logger.Named("pipelines"),
	}
}

// CreateInput contains data needed to create a pipeline.
type CreateInput struct {
	TenantID  uuid.UUID
	Name      string
	IsDefault bool
}

// UpdateInput contains data to update a pipeline.
type UpdateInput struct {
	Name      *string
	IsDefault *bool
}

// StageInput contains data for a pipeline stage.
type StageInput struct {
	TenantID       uuid.UUID
	PipelineID     uuid.UUID
	Name           string
	StageOrder     int
	WinProbability *float64
	IsWonStage     bool
	IsLostStage    bool
	Color          *string
}

// StageUpdateInput contains data to update a pipeline stage.
type StageUpdateInput struct {
	Name           *string
	StageOrder     *int
	WinProbability *float64
	IsWonStage     *bool
	IsLostStage    *bool
	Color          *string
}

// Create creates a new pipeline.
func (s *Service) Create(ctx context.Context, in CreateInput) (*ent.Pipeline, error) {
	result, err := s.client.Pipeline.Create().
		SetTenantID(in.TenantID).
		SetName(in.Name).
		SetIsDefault(in.IsDefault).
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("create pipeline: %w", err)
	}
	s.logger.Info("pipeline created",
		zap.String("id", result.ID.String()),
		zap.String("name", in.Name),
	)
	return result, nil
}

// Get retrieves a pipeline by ID within a tenant.
func (s *Service) Get(ctx context.Context, tenantID, pipelineID uuid.UUID) (*ent.Pipeline, error) {
	result, err := s.client.Pipeline.Query().
		Where(entpipeline.ID(pipelineID), entpipeline.TenantID(tenantID)).
		WithStages(func(q *ent.PipelineStageQuery) {
			q.Order(ent.Asc(entpipelinestage.FieldStageOrder))
		}).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("get pipeline: %w", err)
	}
	return result, nil
}

// List retrieves all pipelines for a tenant.
func (s *Service) List(ctx context.Context, tenantID uuid.UUID) ([]*ent.Pipeline, error) {
	results, err := s.client.Pipeline.Query().
		Where(entpipeline.TenantID(tenantID)).
		WithStages(func(q *ent.PipelineStageQuery) {
			q.Order(ent.Asc(entpipelinestage.FieldStageOrder))
		}).
		Order(ent.Asc(entpipeline.FieldCreatedAt)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("list pipelines: %w", err)
	}
	return results, nil
}

// Update updates a pipeline.
func (s *Service) Update(ctx context.Context, tenantID, pipelineID uuid.UUID, in UpdateInput) (*ent.Pipeline, error) {
	existing, err := s.client.Pipeline.Query().
		Where(entpipeline.ID(pipelineID), entpipeline.TenantID(tenantID)).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("get pipeline: %w", err)
	}
	builder := existing.Update()
	if in.Name != nil {
		builder.SetName(*in.Name)
	}
	if in.IsDefault != nil {
		builder.SetIsDefault(*in.IsDefault)
	}
	result, err := builder.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("update pipeline: %w", err)
	}
	return result, nil
}

// Delete hard-deletes a pipeline.
func (s *Service) Delete(ctx context.Context, tenantID, pipelineID uuid.UUID) error {
	n, err := s.client.Pipeline.Delete().
		Where(entpipeline.ID(pipelineID), entpipeline.TenantID(tenantID)).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("delete pipeline: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("pipeline not found")
	}
	return nil
}

// CreateStage creates a stage within a pipeline.
func (s *Service) CreateStage(ctx context.Context, in StageInput) (*ent.PipelineStage, error) {
	builder := s.client.PipelineStage.Create().
		SetTenantID(in.TenantID).
		SetPipelineID(in.PipelineID).
		SetName(in.Name).
		SetStageOrder(in.StageOrder).
		SetIsWonStage(in.IsWonStage).
		SetIsLostStage(in.IsLostStage)

	if in.WinProbability != nil {
		builder.SetNillableWinProbability(in.WinProbability)
	}
	if in.Color != nil {
		builder.SetNillableColor(in.Color)
	}

	result, err := builder.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("create pipeline stage: %w", err)
	}
	return result, nil
}

// GetStage retrieves a stage by ID.
func (s *Service) GetStage(ctx context.Context, tenantID, stageID uuid.UUID) (*ent.PipelineStage, error) {
	result, err := s.client.PipelineStage.Query().
		Where(entpipelinestage.ID(stageID), entpipelinestage.TenantID(tenantID)).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("get pipeline stage: %w", err)
	}
	return result, nil
}

// UpdateStage updates a pipeline stage.
func (s *Service) UpdateStage(ctx context.Context, tenantID, stageID uuid.UUID, in StageUpdateInput) (*ent.PipelineStage, error) {
	existing, err := s.GetStage(ctx, tenantID, stageID)
	if err != nil {
		return nil, err
	}
	builder := existing.Update()
	if in.Name != nil {
		builder.SetName(*in.Name)
	}
	if in.StageOrder != nil {
		builder.SetStageOrder(*in.StageOrder)
	}
	if in.WinProbability != nil {
		builder.SetNillableWinProbability(in.WinProbability)
	}
	if in.IsWonStage != nil {
		builder.SetIsWonStage(*in.IsWonStage)
	}
	if in.IsLostStage != nil {
		builder.SetIsLostStage(*in.IsLostStage)
	}
	if in.Color != nil {
		builder.SetNillableColor(in.Color)
	}
	result, err := builder.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("update pipeline stage: %w", err)
	}
	return result, nil
}

// DeleteStage hard-deletes a pipeline stage.
func (s *Service) DeleteStage(ctx context.Context, tenantID, stageID uuid.UUID) error {
	n, err := s.client.PipelineStage.Delete().
		Where(entpipelinestage.ID(stageID), entpipelinestage.TenantID(tenantID)).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("delete pipeline stage: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("pipeline stage not found")
	}
	return nil
}
