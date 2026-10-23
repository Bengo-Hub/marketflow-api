package custom_agents

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"entgo.io/ent/dialect/sql"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/agentrun"
	"github.com/bengobox/marketflow-api/internal/ent/customagent"
	"github.com/bengobox/marketflow-api/internal/ent/predicate"
	"github.com/google/uuid"
	nats "github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

// Service handles custom agent management.
type Service struct {
	client   *ent.Client
	logger   *zap.Logger
	natsConn *nats.Conn
}

// NewService creates a new custom agents service.
func NewService(client *ent.Client, logger *zap.Logger, opts ...func(*Service)) *Service {
	s := &Service{
		client: client,
		logger: logger.Named("custom_agents"),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// WithNATS wires a NATS connection for publishing run_requested events.
func WithNATS(nc *nats.Conn) func(*Service) {
	return func(s *Service) { s.natsConn = nc }
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

// notTeamMember excludes Marketing OS team-member overrides, which share the
// custom_agents table (metadata.kind = "team_member") but are managed only through
// /agents/team. COALESCE keeps rows whose metadata is NULL or has no kind.
func notTeamMember() predicate.CustomAgent {
	return func(s *sql.Selector) {
		s.Where(sql.ExprP(fmt.Sprintf("COALESCE(%s->>'kind', '') <> 'team_member'", s.C(customagent.FieldMetadata))))
	}
}

// Get retrieves a custom agent by ID.
func (s *Service) Get(ctx context.Context, tenantID, agentID uuid.UUID) (*ent.CustomAgent, error) {
	result, err := s.client.CustomAgent.Query().
		Where(customagent.ID(agentID), customagent.TenantID(tenantID), notTeamMember()).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("get custom agent: %w", err)
	}
	return result, nil
}

// List retrieves all custom agents for a tenant.
func (s *Service) List(ctx context.Context, tenantID uuid.UUID) ([]*ent.CustomAgent, error) {
	results, err := s.client.CustomAgent.Query().
		Where(customagent.TenantID(tenantID), notTeamMember()).
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
		Where(customagent.ID(agentID), customagent.TenantID(tenantID), notTeamMember()).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("delete custom agent: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("custom agent not found")
	}
	return nil
}

// TriggerRun creates a pending AgentRun record and publishes crm.agent.run_requested.
func (s *Service) TriggerRun(ctx context.Context, tenantID, agentID uuid.UUID, triggeredBy string, payload map[string]any) (*ent.AgentRun, error) {
	if _, err := s.Get(ctx, tenantID, agentID); err != nil {
		return nil, fmt.Errorf("custom agent not found")
	}
	if payload == nil {
		payload = map[string]any{}
	}
	run, err := s.client.AgentRun.Create().
		SetTenantID(tenantID).
		SetCustomAgentID(agentID).
		SetTriggeredBy(triggeredBy).
		SetTriggerPayload(payload).
		SetStatus("pending").
		SetTrace(map[string]any{}).
		SetTotalTokens(0).
		SetToolCallsN(0).
		SetStartedAt(time.Now()).
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("create agent run: %w", err)
	}

	if s.natsConn != nil {
		event := map[string]any{
			"run_id":    run.ID.String(),
			"agent_id":  agentID.String(),
			"tenant_id": tenantID.String(),
			"payload":   payload,
		}
		b, _ := json.Marshal(event)
		// JetStream (acked) publish: the worker's CustomAgentExecutionConsumer is a
		// durable JS consumer — a core publish is unacked, and a lost signal strands
		// this AgentRun in "pending" forever. On publish failure the run is marked
		// failed so it never hangs.
		var pubErr error
		if js, jsErr := s.natsConn.JetStream(); jsErr == nil {
			_, pubErr = js.Publish("crm.agent.run_requested", b)
		} else {
			pubErr = s.natsConn.Publish("crm.agent.run_requested", b)
		}
		if pubErr != nil {
			s.logger.Warn("failed to publish crm.agent.run_requested", zap.Error(pubErr))
			if failed, uErr := run.Update().
				SetStatus("failed").
				SetErrorMessage("dispatch failed: " + pubErr.Error()).
				Save(ctx); uErr == nil {
				return failed, nil
			}
		}
	}
	return run, nil
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
