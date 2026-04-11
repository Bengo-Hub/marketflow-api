package meetings

import (
	"context"
	"fmt"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/scheduledmeeting"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Service handles scheduled meeting business logic.
type Service struct {
	client *ent.Client
	logger *zap.Logger
}

// NewService creates a new scheduled meeting service.
func NewService(client *ent.Client, logger *zap.Logger) *Service {
	return &Service{
		client: client,
		logger: logger.Named("meetings"),
	}
}

// CreateInput contains the data needed to create a scheduled meeting.
type CreateInput struct {
	TenantID        uuid.UUID
	LeadID          *uuid.UUID
	ContactID       *uuid.UUID
	CalBookingID    *string
	MeetingURL      *string
	ScheduledAt     time.Time
	DurationMinutes *int
	MeetingType     *string
	Notes           *string
	Metadata        map[string]any
}

// UpdateInput contains the data to update a scheduled meeting.
type UpdateInput struct {
	LeadID          *uuid.UUID
	ContactID       *uuid.UUID
	CalBookingID    *string
	MeetingURL      *string
	ScheduledAt     *time.Time
	DurationMinutes *int
	MeetingType     *string
	Status          *string
	Notes           *string
	Metadata        map[string]any
}

// ListInput contains filtering/pagination for listing meetings.
type ListInput struct {
	TenantID    uuid.UUID
	Status      *string
	MeetingType *string
	DateFrom    *time.Time
	DateTo      *time.Time
	Limit       int
	Offset      int
}

// Create creates a new scheduled meeting.
func (s *Service) Create(ctx context.Context, in CreateInput) (*ent.ScheduledMeeting, error) {
	builder := s.client.ScheduledMeeting.Create().
		SetTenantID(in.TenantID).
		SetScheduledAt(in.ScheduledAt)

	if in.LeadID != nil {
		builder.SetNillableLeadID(in.LeadID)
	}
	if in.ContactID != nil {
		builder.SetNillableContactID(in.ContactID)
	}
	if in.CalBookingID != nil {
		builder.SetNillableCalBookingID(in.CalBookingID)
	}
	if in.MeetingURL != nil {
		builder.SetNillableMeetingURL(in.MeetingURL)
	}
	if in.DurationMinutes != nil {
		builder.SetNillableDurationMinutes(in.DurationMinutes)
	}
	if in.MeetingType != nil {
		builder.SetNillableMeetingType(in.MeetingType)
	}
	if in.Notes != nil {
		builder.SetNillableNotes(in.Notes)
	}
	if in.Metadata != nil {
		builder.SetMetadata(in.Metadata)
	}

	result, err := builder.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("create meeting: %w", err)
	}

	s.logger.Info("meeting created",
		zap.String("id", result.ID.String()),
		zap.String("tenant_id", in.TenantID.String()),
	)

	return result, nil
}

// Get retrieves a scheduled meeting by ID within a tenant.
func (s *Service) Get(ctx context.Context, tenantID, meetingID uuid.UUID) (*ent.ScheduledMeeting, error) {
	result, err := s.client.ScheduledMeeting.Query().
		Where(
			scheduledmeeting.ID(meetingID),
			scheduledmeeting.TenantID(tenantID),
		).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("get meeting: %w", err)
	}
	return result, nil
}

// List retrieves scheduled meetings with filtering and pagination.
func (s *Service) List(ctx context.Context, in ListInput) ([]*ent.ScheduledMeeting, int, error) {
	query := s.client.ScheduledMeeting.Query().
		Where(scheduledmeeting.TenantID(in.TenantID))

	if in.Status != nil {
		query = query.Where(scheduledmeeting.StatusEQ(*in.Status))
	}
	if in.MeetingType != nil {
		query = query.Where(scheduledmeeting.MeetingTypeEQ(*in.MeetingType))
	}
	if in.DateFrom != nil {
		query = query.Where(scheduledmeeting.ScheduledAtGTE(*in.DateFrom))
	}
	if in.DateTo != nil {
		query = query.Where(scheduledmeeting.ScheduledAtLTE(*in.DateTo))
	}

	total, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("count meetings: %w", err)
	}

	if in.Limit <= 0 {
		in.Limit = 20
	}
	if in.Limit > 100 {
		in.Limit = 100
	}

	results, err := query.
		Order(ent.Desc(scheduledmeeting.FieldScheduledAt)).
		Limit(in.Limit).
		Offset(in.Offset).
		All(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("list meetings: %w", err)
	}

	return results, total, nil
}

// Update updates a scheduled meeting.
func (s *Service) Update(ctx context.Context, tenantID, meetingID uuid.UUID, in UpdateInput) (*ent.ScheduledMeeting, error) {
	existing, err := s.Get(ctx, tenantID, meetingID)
	if err != nil {
		return nil, err
	}

	builder := existing.Update()

	if in.LeadID != nil {
		builder.SetNillableLeadID(in.LeadID)
	}
	if in.ContactID != nil {
		builder.SetNillableContactID(in.ContactID)
	}
	if in.CalBookingID != nil {
		builder.SetNillableCalBookingID(in.CalBookingID)
	}
	if in.MeetingURL != nil {
		builder.SetNillableMeetingURL(in.MeetingURL)
	}
	if in.ScheduledAt != nil {
		builder.SetScheduledAt(*in.ScheduledAt)
	}
	if in.DurationMinutes != nil {
		builder.SetDurationMinutes(*in.DurationMinutes)
	}
	if in.MeetingType != nil {
		builder.SetMeetingType(*in.MeetingType)
	}
	if in.Status != nil {
		builder.SetStatus(*in.Status)
	}
	if in.Notes != nil {
		builder.SetNillableNotes(in.Notes)
	}
	if in.Metadata != nil {
		builder.SetMetadata(in.Metadata)
	}

	result, err := builder.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("update meeting: %w", err)
	}

	return result, nil
}
