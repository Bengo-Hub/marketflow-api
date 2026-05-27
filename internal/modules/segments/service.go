package segments

import (
	"context"
	"fmt"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	entcontact "github.com/bengobox/marketflow-api/internal/ent/contact"
	entlead "github.com/bengobox/marketflow-api/internal/ent/lead"
	entsegment "github.com/bengobox/marketflow-api/internal/ent/segment"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Service handles segment business logic.
type Service struct {
	client *ent.Client
	logger *zap.Logger
}

// NewService creates a new segment service.
func NewService(client *ent.Client, logger *zap.Logger) *Service {
	return &Service{client: client, logger: logger.Named("segments")}
}

// CreateInput holds segment creation data.
type CreateInput struct {
	TenantID    uuid.UUID
	Name        string
	EntityType  string
	FilterRules map[string]any
	IsDynamic   bool
}

// UpdateInput holds segment update data.
type UpdateInput struct {
	Name        *string
	FilterRules map[string]any
	IsDynamic   *bool
}

// Create creates a new segment.
func (s *Service) Create(ctx context.Context, in CreateInput) (*ent.Segment, error) {
	entityType := in.EntityType
	if entityType == "" {
		entityType = "contact"
	}
	rules := in.FilterRules
	if rules == nil {
		rules = map[string]any{}
	}
	seg, err := s.client.Segment.Create().
		SetTenantID(in.TenantID).
		SetName(in.Name).
		SetEntityType(entityType).
		SetFilterRules(rules).
		SetIsDynamic(in.IsDynamic).
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("create segment: %w", err)
	}
	return seg, nil
}

// List returns paginated segments for a tenant.
func (s *Service) List(ctx context.Context, tenantID uuid.UUID, limit, offset int) ([]*ent.Segment, int, error) {
	q := s.client.Segment.Query().Where(entsegment.TenantID(tenantID))
	total, _ := q.Clone().Count(ctx)
	segs, err := q.Order(ent.Asc(entsegment.FieldCreatedAt)).Limit(limit).Offset(offset).All(ctx)
	return segs, total, err
}

// Get returns a single segment.
func (s *Service) Get(ctx context.Context, tenantID, id uuid.UUID) (*ent.Segment, error) {
	seg, err := s.client.Segment.Query().
		Where(entsegment.ID(id), entsegment.TenantID(tenantID)).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("segment not found: %w", err)
	}
	return seg, nil
}

// Update updates mutable segment fields.
func (s *Service) Update(ctx context.Context, tenantID, id uuid.UUID, in UpdateInput) (*ent.Segment, error) {
	upd := s.client.Segment.Update().
		Where(entsegment.ID(id), entsegment.TenantID(tenantID))
	if in.Name != nil {
		upd = upd.SetName(*in.Name)
	}
	if in.FilterRules != nil {
		upd = upd.SetFilterRules(in.FilterRules)
	}
	if in.IsDynamic != nil {
		upd = upd.SetIsDynamic(*in.IsDynamic)
	}
	if _, err := upd.Save(ctx); err != nil {
		return nil, fmt.Errorf("update segment: %w", err)
	}
	return s.Get(ctx, tenantID, id)
}

// Delete hard-deletes a segment.
func (s *Service) Delete(ctx context.Context, tenantID, id uuid.UUID) error {
	n, err := s.client.Segment.Delete().
		Where(entsegment.ID(id), entsegment.TenantID(tenantID)).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("delete segment: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("segment not found")
	}
	return nil
}

// Evaluate re-runs filter rules and updates member_count.
// Returns the updated segment.
func (s *Service) Evaluate(ctx context.Context, tenantID, id uuid.UUID) (*ent.Segment, error) {
	seg, err := s.Get(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}

	count, err := s.evaluateRules(ctx, tenantID, seg.EntityType, seg.FilterRules)
	if err != nil {
		s.logger.Warn("segment evaluation failed", zap.String("id", id.String()), zap.Error(err))
		count = 0
	}

	now := time.Now()
	updated, err := s.client.Segment.UpdateOneID(id).
		SetMemberCount(count).
		SetLastCalculatedAt(now).
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("update member count: %w", err)
	}
	return updated, nil
}

// evaluateRules counts matching entities based on filter_rules.
// rules format: {"conditions": [{"field":"lifecycle_stage","op":"eq","value":"customer"}]}
func (s *Service) evaluateRules(ctx context.Context, tenantID uuid.UUID, entityType string, rules map[string]any) (int, error) {
	switch entityType {
	case "contact":
		q := s.client.Contact.Query().Where(entcontact.TenantID(tenantID))
		return q.Count(ctx)
	case "lead":
		q := s.client.Lead.Query().Where(entlead.TenantID(tenantID))
		return q.Count(ctx)
	default:
		return 0, fmt.Errorf("unsupported entity_type: %s", entityType)
	}
}
