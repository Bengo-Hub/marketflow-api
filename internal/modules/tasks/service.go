package tasks

import (
	"context"
	"fmt"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/task"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Service handles task business logic.
type Service struct {
	client *ent.Client
	logger *zap.Logger
}

// NewService creates a new task service.
func NewService(client *ent.Client, logger *zap.Logger) *Service {
	return &Service{
		client: client,
		logger: logger.Named("tasks"),
	}
}

// CreateInput contains data needed to create a task.
type CreateInput struct {
	TenantID         uuid.UUID
	EntityType       string
	EntityID         uuid.UUID
	Title            string
	Description      *string
	DueAt            *time.Time
	Priority         string
	AssignedToUserID *uuid.UUID
	CreatedBy        *uuid.UUID
}

// UpdateInput contains data to update a task.
type UpdateInput struct {
	Title            *string
	Description      *string
	DueAt            *time.Time
	Priority         *string
	Status           *string
	AssignedToUserID *uuid.UUID
}

// ListInput contains filtering for listing tasks.
type ListInput struct {
	TenantID   uuid.UUID
	EntityType *string
	EntityID   *uuid.UUID
	Status     *string
	AssignedTo *uuid.UUID
	Overdue    bool
	Limit      int
	Offset     int
}

// Create creates a new task.
func (s *Service) Create(ctx context.Context, in CreateInput) (*ent.Task, error) {
	priority := in.Priority
	if priority == "" {
		priority = "medium"
	}

	builder := s.client.Task.Create().
		SetTenantID(in.TenantID).
		SetEntityType(in.EntityType).
		SetEntityID(in.EntityID).
		SetTitle(in.Title).
		SetPriority(priority)

	if in.Description != nil {
		builder.SetNillableDescription(in.Description)
	}
	if in.DueAt != nil {
		builder.SetNillableDueAt(in.DueAt)
	}
	if in.AssignedToUserID != nil {
		builder.SetNillableAssignedToUserID(in.AssignedToUserID)
	}
	if in.CreatedBy != nil {
		builder.SetNillableCreatedBy(in.CreatedBy)
	}

	result, err := builder.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("create task: %w", err)
	}
	s.logger.Info("task created",
		zap.String("id", result.ID.String()),
		zap.String("title", in.Title),
	)
	return result, nil
}

// Get retrieves a task by ID.
func (s *Service) Get(ctx context.Context, tenantID, taskID uuid.UUID) (*ent.Task, error) {
	result, err := s.client.Task.Query().
		Where(task.ID(taskID), task.TenantID(tenantID)).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("get task: %w", err)
	}
	return result, nil
}

// List retrieves tasks with filtering and pagination.
func (s *Service) List(ctx context.Context, in ListInput) ([]*ent.Task, int, error) {
	query := s.client.Task.Query().Where(task.TenantID(in.TenantID))

	if in.EntityType != nil {
		query = query.Where(task.EntityTypeEQ(*in.EntityType))
	}
	if in.EntityID != nil {
		query = query.Where(task.EntityID(*in.EntityID))
	}
	if in.Status != nil {
		query = query.Where(task.StatusEQ(*in.Status))
	}
	if in.AssignedTo != nil {
		query = query.Where(task.AssignedToUserID(*in.AssignedTo))
	}
	if in.Overdue {
		query = query.Where(
			task.DueAtLT(time.Now()),
			task.StatusNEQ("done"),
		)
	}

	total, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("count tasks: %w", err)
	}

	if in.Limit <= 0 {
		in.Limit = 20
	}
	if in.Limit > 100 {
		in.Limit = 100
	}

	results, err := query.
		Order(ent.Asc(task.FieldDueAt)).
		Limit(in.Limit).
		Offset(in.Offset).
		All(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("list tasks: %w", err)
	}
	return results, total, nil
}

// Update updates a task.
func (s *Service) Update(ctx context.Context, tenantID, taskID uuid.UUID, in UpdateInput) (*ent.Task, error) {
	existing, err := s.Get(ctx, tenantID, taskID)
	if err != nil {
		return nil, err
	}
	builder := existing.Update()
	if in.Title != nil {
		builder.SetTitle(*in.Title)
	}
	if in.Description != nil {
		builder.SetNillableDescription(in.Description)
	}
	if in.DueAt != nil {
		builder.SetNillableDueAt(in.DueAt)
	}
	if in.Priority != nil {
		builder.SetPriority(*in.Priority)
	}
	if in.Status != nil {
		builder.SetStatus(*in.Status)
	}
	if in.AssignedToUserID != nil {
		builder.SetNillableAssignedToUserID(in.AssignedToUserID)
	}
	result, err := builder.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("update task: %w", err)
	}
	return result, nil
}

// Complete marks a task as done.
func (s *Service) Complete(ctx context.Context, tenantID, taskID uuid.UUID) (*ent.Task, error) {
	existing, err := s.Get(ctx, tenantID, taskID)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	result, err := existing.Update().
		SetStatus("done").
		SetNillableCompletedAt(&now).
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("complete task: %w", err)
	}
	return result, nil
}

// Delete hard-deletes a task.
func (s *Service) Delete(ctx context.Context, tenantID, taskID uuid.UUID) error {
	n, err := s.client.Task.Delete().
		Where(task.ID(taskID), task.TenantID(tenantID)).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("delete task: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("task not found")
	}
	return nil
}
