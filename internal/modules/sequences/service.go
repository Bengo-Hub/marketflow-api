package sequences

import (
	"context"
	"fmt"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/nurtureenrollment"
	"github.com/bengobox/marketflow-api/internal/ent/nurturesequence"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Service handles nurture sequence business logic.
type Service struct {
	client *ent.Client
	logger *zap.Logger
}

// NewService creates a new nurture sequence service.
func NewService(client *ent.Client, logger *zap.Logger) *Service {
	return &Service{
		client: client,
		logger: logger.Named("sequences"),
	}
}

// CreateInput contains the data needed to create a nurture sequence.
type CreateInput struct {
	TenantID       uuid.UUID
	Name           string
	TriggerEvent   string
	LeadTypeFilter *string
	Active         *bool
	Steps          []map[string]any
	Metadata       map[string]any
}

// UpdateInput contains the data to update a nurture sequence.
type UpdateInput struct {
	Name           *string
	TriggerEvent   *string
	LeadTypeFilter *string
	Active         *bool
	Steps          []map[string]any
	Metadata       map[string]any
}

// ListInput contains filtering/pagination for listing sequences.
type ListInput struct {
	TenantID     uuid.UUID
	TriggerEvent *string
	Active       *bool
	Limit        int
	Offset       int
}

// Create creates a new nurture sequence.
func (s *Service) Create(ctx context.Context, in CreateInput) (*ent.NurtureSequence, error) {
	builder := s.client.NurtureSequence.Create().
		SetTenantID(in.TenantID).
		SetName(in.Name).
		SetTriggerEvent(in.TriggerEvent)

	if in.LeadTypeFilter != nil {
		builder.SetNillableLeadTypeFilter(in.LeadTypeFilter)
	}
	if in.Active != nil {
		builder.SetNillableActive(in.Active)
	}
	if in.Steps != nil {
		builder.SetSteps(in.Steps)
	}
	if in.Metadata != nil {
		builder.SetMetadata(in.Metadata)
	}

	result, err := builder.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("create sequence: %w", err)
	}

	s.logger.Info("nurture sequence created",
		zap.String("id", result.ID.String()),
		zap.String("tenant_id", in.TenantID.String()),
		zap.String("trigger_event", in.TriggerEvent),
	)

	return result, nil
}

// Get retrieves a nurture sequence by ID within a tenant.
func (s *Service) Get(ctx context.Context, tenantID, sequenceID uuid.UUID) (*ent.NurtureSequence, error) {
	result, err := s.client.NurtureSequence.Query().
		Where(
			nurturesequence.ID(sequenceID),
			nurturesequence.TenantID(tenantID),
		).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("get sequence: %w", err)
	}
	return result, nil
}

// List retrieves nurture sequences with filtering and pagination.
func (s *Service) List(ctx context.Context, in ListInput) ([]*ent.NurtureSequence, int, error) {
	query := s.client.NurtureSequence.Query().
		Where(nurturesequence.TenantID(in.TenantID))

	if in.TriggerEvent != nil {
		query = query.Where(nurturesequence.TriggerEventEQ(*in.TriggerEvent))
	}
	if in.Active != nil {
		query = query.Where(nurturesequence.ActiveEQ(*in.Active))
	}

	total, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("count sequences: %w", err)
	}

	if in.Limit <= 0 {
		in.Limit = 20
	}
	if in.Limit > 100 {
		in.Limit = 100
	}

	results, err := query.
		Order(ent.Desc(nurturesequence.FieldCreatedAt)).
		Limit(in.Limit).
		Offset(in.Offset).
		All(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("list sequences: %w", err)
	}

	return results, total, nil
}

// Update updates a nurture sequence.
func (s *Service) Update(ctx context.Context, tenantID, sequenceID uuid.UUID, in UpdateInput) (*ent.NurtureSequence, error) {
	existing, err := s.Get(ctx, tenantID, sequenceID)
	if err != nil {
		return nil, err
	}

	builder := existing.Update()

	if in.Name != nil {
		builder.SetName(*in.Name)
	}
	if in.TriggerEvent != nil {
		builder.SetTriggerEvent(*in.TriggerEvent)
	}
	if in.LeadTypeFilter != nil {
		builder.SetNillableLeadTypeFilter(in.LeadTypeFilter)
	}
	if in.Active != nil {
		builder.SetActive(*in.Active)
	}
	if in.Steps != nil {
		builder.SetSteps(in.Steps)
	}
	if in.Metadata != nil {
		builder.SetMetadata(in.Metadata)
	}

	result, err := builder.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("update sequence: %w", err)
	}

	return result, nil
}

// SequenceAnalytics contains enrollment analytics for a sequence.
type SequenceAnalytics struct {
	TotalEnrolled  int     `json:"total_enrolled"`
	Active         int     `json:"active"`
	Completed      int     `json:"completed"`
	Dropped        int     `json:"dropped"`
	CompletionRate float64 `json:"completion_rate"`
}

// Analytics returns enrollment analytics for a given sequence.
func (s *Service) Analytics(ctx context.Context, tenantID, sequenceID uuid.UUID) (*SequenceAnalytics, error) {
	// Verify the sequence exists and belongs to the tenant.
	_, err := s.Get(ctx, tenantID, sequenceID)
	if err != nil {
		return nil, err
	}

	baseQuery := s.client.NurtureEnrollment.Query().
		Where(
			nurtureenrollment.TenantIDEQ(tenantID),
			nurtureenrollment.SequenceIDEQ(sequenceID),
		)

	totalEnrolled, _ := baseQuery.Clone().Count(ctx)
	active, _ := baseQuery.Clone().Where(nurtureenrollment.StatusEQ("active")).Count(ctx)
	completed, _ := baseQuery.Clone().Where(nurtureenrollment.StatusEQ("completed")).Count(ctx)
	dropped, _ := baseQuery.Clone().Where(nurtureenrollment.StatusEQ("dropped")).Count(ctx)

	completionRate := 0.0
	if totalEnrolled > 0 {
		completionRate = float64(completed) / float64(totalEnrolled) * 100
	}

	return &SequenceAnalytics{
		TotalEnrolled:  totalEnrolled,
		Active:         active,
		Completed:      completed,
		Dropped:        dropped,
		CompletionRate: completionRate,
	}, nil
}

// Delete deletes a nurture sequence by setting active=false.
func (s *Service) Delete(ctx context.Context, tenantID, sequenceID uuid.UUID) error {
	_, err := s.client.NurtureSequence.Update().
		Where(
			nurturesequence.ID(sequenceID),
			nurturesequence.TenantID(tenantID),
		).
		SetActive(false).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("delete sequence: %w", err)
	}
	return nil
}
