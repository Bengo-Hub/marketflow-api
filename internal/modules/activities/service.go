package activities

import (
	"context"
	"fmt"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/activity"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Service handles activity timeline business logic.
type Service struct {
	client *ent.Client
	logger *zap.Logger
}

// NewService creates a new activity service.
func NewService(client *ent.Client, logger *zap.Logger) *Service {
	return &Service{
		client: client,
		logger: logger.Named("activities"),
	}
}

// CreateInput contains data to log an activity.
type CreateInput struct {
	TenantID        uuid.UUID
	EntityType      string // lead | contact | deal
	EntityID        uuid.UUID
	ActivityType    string // email | call | note | meeting | sms | whatsapp | order | payment | ticket_opened | ticket_resolved
	Direction       *string
	Subject         *string
	Body            *string
	Outcome         *string
	DurationMinutes *int
	PerformedBy     *uuid.UUID
	Source          *string
	ExternalRef     *string
	PerformedAt     *time.Time
}

// ListInput contains filtering for listing activities for an entity.
type ListInput struct {
	TenantID     uuid.UUID
	EntityType   string
	EntityID     uuid.UUID
	ActivityType *string
	Limit        int
	Offset       int
}

// Log creates a new activity entry.
func (s *Service) Log(ctx context.Context, in CreateInput) (*ent.Activity, error) {
	performedAt := time.Now()
	if in.PerformedAt != nil {
		performedAt = *in.PerformedAt
	}

	builder := s.client.Activity.Create().
		SetTenantID(in.TenantID).
		SetEntityType(in.EntityType).
		SetEntityID(in.EntityID).
		SetActivityType(in.ActivityType).
		SetPerformedAt(performedAt)

	if in.Direction != nil {
		builder.SetNillableDirection(in.Direction)
	}
	if in.Subject != nil {
		builder.SetNillableSubject(in.Subject)
	}
	if in.Body != nil {
		builder.SetNillableBody(in.Body)
	}
	if in.Outcome != nil {
		builder.SetNillableOutcome(in.Outcome)
	}
	if in.DurationMinutes != nil {
		builder.SetNillableDurationMinutes(in.DurationMinutes)
	}
	if in.PerformedBy != nil {
		builder.SetNillablePerformedBy(in.PerformedBy)
	}
	if in.Source != nil {
		builder.SetNillableSource(in.Source)
	}
	if in.ExternalRef != nil {
		builder.SetNillableExternalRef(in.ExternalRef)
	}

	result, err := builder.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("log activity: %w", err)
	}
	s.logger.Info("activity logged",
		zap.String("entity_type", in.EntityType),
		zap.String("entity_id", in.EntityID.String()),
		zap.String("type", in.ActivityType),
	)
	return result, nil
}

// List retrieves activities for a specific entity (polymorphic timeline).
func (s *Service) List(ctx context.Context, in ListInput) ([]*ent.Activity, int, error) {
	query := s.client.Activity.Query().
		Where(
			activity.TenantID(in.TenantID),
			activity.EntityTypeEQ(in.EntityType),
			activity.EntityID(in.EntityID),
		)

	if in.ActivityType != nil {
		query = query.Where(activity.ActivityTypeEQ(*in.ActivityType))
	}

	total, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("count activities: %w", err)
	}

	if in.Limit <= 0 {
		in.Limit = 50
	}
	if in.Limit > 200 {
		in.Limit = 200
	}

	results, err := query.
		Order(ent.Desc(activity.FieldPerformedAt)).
		Limit(in.Limit).
		Offset(in.Offset).
		All(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("list activities: %w", err)
	}
	return results, total, nil
}

// Get retrieves a single activity by ID.
func (s *Service) Get(ctx context.Context, tenantID, activityID uuid.UUID) (*ent.Activity, error) {
	result, err := s.client.Activity.Query().
		Where(activity.ID(activityID), activity.TenantID(tenantID)).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("get activity: %w", err)
	}
	return result, nil
}
