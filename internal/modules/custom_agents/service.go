package custom_agents

import (
	"context"
	"fmt"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/agentrun"
	"github.com/bengobox/marketflow-api/internal/ent/customagent"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Service handles custom agent management.
type Service struct {
	client *ent.Client
	logger *zap.Logger
}

// NewService creates a new custom agents service.
func NewService(client *ent.Client, logger *zap.Logger) *Service {
	return &Service{
		client: client,
		logger: logger.Named("custom_agents"),
	}
}

// CreateInput contains data needed to create a custom agent.
type CreateInput struct {
	TenantID      uuid.UUID
	Name          string
	Description   *string
	TriggerType   string // event | cron | manual
	TriggerConfig map[string]any
	SystemPrompt  *string
	AllowedTools  []string
	MaxIterations int
}

// UpdateInput contains data to update a custom agent.
type UpdateInput struct {
	Name          *string
	Description   *string
	TriggerType   *string
	TriggerConfig map[string]any
	SystemPrompt  *string
	AllowedTools  []string
	MaxIterations *int
	IsActive      *bool
}

// Create creates a new custom agent.
func (s *Service) Create(ctx context.Context, in CreateInput) (*ent.CustomAgent, error) {
	maxIter := in.MaxIterations
	if maxIter <= 0 {
		maxIter = 10
	}

	builder := s.client.CustomAgent.Create().
		SetTenantID(in.TenantID).
		SetName(in.Name).
		SetTriggerType(in.TriggerType).
		SetMaxIterations(maxIter).
		SetIsActive(false)

	if in.Description != nil {
		builder.SetNillableDescription(in.Description)
	}
	if in.TriggerConfig != nil {
		builder.SetTriggerConfig(in.TriggerConfig)
	}
	if in.SystemPrompt != nil {
		builder.SetNillableSystemPrompt(in.SystemPrompt)
	}
	if len(in.AllowedTools) > 0 {
		builder.SetAllowedTools(in.AllowedTools)
	}

	result, err := builder.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("create custom agent: %w", err)
	}
	s.logger.Info("custom agent created",
		zap.String("id", result.ID.String()),
		zap.String("name", in.Name),
	)
	return result, nil
}

// Get retrieves a custom agent by ID.
func (s *Service) Get(ctx context.Context, tenantID, agentID uuid.UUID) (*ent.CustomAgent, error) {
	result, err := s.client.CustomAgent.Query().
		Where(customagent.ID(agentID), customagent.TenantID(tenantID)).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("get custom agent: %w", err)
	}
	return result, nil
}

// List retrieves all custom agents for a tenant.
func (s *Service) List(ctx context.Context, tenantID uuid.UUID) ([]*ent.CustomAgent, error) {
	results, err := s.client.CustomAgent.Query().
		Where(customagent.TenantID(tenantID)).
		Order(ent.Asc(customagent.FieldCreatedAt)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("list custom agents: %w", err)
	}
	return results, nil
}

// Update updates a custom agent.
func (s *Service) Update(ctx context.Context, tenantID, agentID uuid.UUID, in UpdateInput) (*ent.CustomAgent, error) {
	existing, err := s.Get(ctx, tenantID, agentID)
	if err != nil {
		return nil, err
	}
	builder := existing.Update()
	if in.Name != nil {
		builder.SetName(*in.Name)
	}
	if in.Description != nil {
		builder.SetNillableDescription(in.Description)
	}
	if in.TriggerType != nil {
		builder.SetTriggerType(*in.TriggerType)
	}
	if in.TriggerConfig != nil {
		builder.SetTriggerConfig(in.TriggerConfig)
	}
	if in.SystemPrompt != nil {
		builder.SetNillableSystemPrompt(in.SystemPrompt)
	}
	if len(in.AllowedTools) > 0 {
		builder.SetAllowedTools(in.AllowedTools)
	}
	if in.MaxIterations != nil {
		builder.SetMaxIterations(*in.MaxIterations)
	}
	if in.IsActive != nil {
		builder.SetIsActive(*in.IsActive)
	}
	result, err := builder.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("update custom agent: %w", err)
	}
	return result, nil
}

// Delete hard-deletes a custom agent.
func (s *Service) Delete(ctx context.Context, tenantID, agentID uuid.UUID) error {
	n, err := s.client.CustomAgent.Delete().
		Where(customagent.ID(agentID), customagent.TenantID(tenantID)).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("delete custom agent: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("custom agent not found")
	}
	return nil
}

// ListRuns retrieves run history for a custom agent.
func (s *Service) ListRuns(ctx context.Context, tenantID, agentID uuid.UUID, limit, offset int) ([]*ent.AgentRun, int, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	query := s.client.AgentRun.Query().
		Where(
			agentrun.TenantID(tenantID),
			agentrun.CustomAgentID(agentID),
		)

	total, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("count agent runs: %w", err)
	}

	results, err := query.
		Order(ent.Desc(agentrun.FieldStartedAt)).
		Limit(limit).
		Offset(offset).
		All(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("list agent runs: %w", err)
	}
	return results, total, nil
}
