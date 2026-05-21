package custom_fields

import (
	"context"
	"fmt"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/customfielddef"
	"github.com/bengobox/marketflow-api/internal/ent/customfieldvalue"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Service handles custom field definitions and values.
type Service struct {
	client *ent.Client
	logger *zap.Logger
}

// NewService creates a new custom fields service.
func NewService(client *ent.Client, logger *zap.Logger) *Service {
	return &Service{
		client: client,
		logger: logger.Named("custom_fields"),
	}
}

// CreateDefInput contains data to create a custom field definition.
type CreateDefInput struct {
	TenantID   uuid.UUID
	EntityType string
	FieldKey   string
	FieldLabel string
	FieldType  string // text | number | date | select | multiselect | boolean
	Options    []string
	IsRequired bool
	SortOrder  int
}

// UpdateDefInput contains data to update a custom field definition.
type UpdateDefInput struct {
	FieldLabel *string
	Options    []string
	IsRequired *bool
	SortOrder  *int
}

// CreateDef creates a custom field definition.
func (s *Service) CreateDef(ctx context.Context, in CreateDefInput) (*ent.CustomFieldDef, error) {
	builder := s.client.CustomFieldDef.Create().
		SetTenantID(in.TenantID).
		SetEntityType(in.EntityType).
		SetFieldKey(in.FieldKey).
		SetFieldLabel(in.FieldLabel).
		SetFieldType(in.FieldType).
		SetIsRequired(in.IsRequired).
		SetSortOrder(in.SortOrder)

	if len(in.Options) > 0 {
		builder.SetOptions(in.Options)
	}

	result, err := builder.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("create custom field def: %w", err)
	}
	s.logger.Info("custom field def created",
		zap.String("entity_type", in.EntityType),
		zap.String("field_key", in.FieldKey),
	)
	return result, nil
}

// GetDef retrieves a custom field definition by ID.
func (s *Service) GetDef(ctx context.Context, tenantID, defID uuid.UUID) (*ent.CustomFieldDef, error) {
	result, err := s.client.CustomFieldDef.Query().
		Where(customfielddef.ID(defID), customfielddef.TenantID(tenantID)).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("get custom field def: %w", err)
	}
	return result, nil
}

// ListDefs lists custom field definitions for a tenant, optionally filtered by entity type.
func (s *Service) ListDefs(ctx context.Context, tenantID uuid.UUID, entityType *string) ([]*ent.CustomFieldDef, error) {
	query := s.client.CustomFieldDef.Query().
		Where(customfielddef.TenantID(tenantID))

	if entityType != nil {
		query = query.Where(customfielddef.EntityTypeEQ(*entityType))
	}

	results, err := query.
		Order(ent.Asc(customfielddef.FieldSortOrder)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("list custom field defs: %w", err)
	}
	return results, nil
}

// UpdateDef updates a custom field definition.
func (s *Service) UpdateDef(ctx context.Context, tenantID, defID uuid.UUID, in UpdateDefInput) (*ent.CustomFieldDef, error) {
	existing, err := s.GetDef(ctx, tenantID, defID)
	if err != nil {
		return nil, err
	}
	builder := existing.Update()
	if in.FieldLabel != nil {
		builder.SetFieldLabel(*in.FieldLabel)
	}
	if len(in.Options) > 0 {
		builder.SetOptions(in.Options)
	}
	if in.IsRequired != nil {
		builder.SetIsRequired(*in.IsRequired)
	}
	if in.SortOrder != nil {
		builder.SetSortOrder(*in.SortOrder)
	}
	result, err := builder.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("update custom field def: %w", err)
	}
	return result, nil
}

// DeleteDef hard-deletes a custom field definition.
func (s *Service) DeleteDef(ctx context.Context, tenantID, defID uuid.UUID) error {
	n, err := s.client.CustomFieldDef.Delete().
		Where(customfielddef.ID(defID), customfielddef.TenantID(tenantID)).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("delete custom field def: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("custom field def not found")
	}
	return nil
}

// SetValueInput contains data to set a custom field value on an entity.
type SetValueInput struct {
	TenantID   uuid.UUID
	EntityType string
	EntityID   uuid.UUID
	FieldDefID uuid.UUID
	Value      string
}

// SetValue upserts a custom field value for an entity.
func (s *Service) SetValue(ctx context.Context, in SetValueInput) (*ent.CustomFieldValue, error) {
	existing, err := s.client.CustomFieldValue.Query().
		Where(
			customfieldvalue.TenantID(in.TenantID),
			customfieldvalue.EntityTypeEQ(in.EntityType),
			customfieldvalue.EntityID(in.EntityID),
			customfieldvalue.FieldDefID(in.FieldDefID),
		).
		Only(ctx)

	if err == nil {
		// Update existing value
		result, err := existing.Update().SetValue(in.Value).Save(ctx)
		if err != nil {
			return nil, fmt.Errorf("update custom field value: %w", err)
		}
		return result, nil
	}

	// Create new value
	result, err := s.client.CustomFieldValue.Create().
		SetTenantID(in.TenantID).
		SetEntityType(in.EntityType).
		SetEntityID(in.EntityID).
		SetFieldDefID(in.FieldDefID).
		SetValue(in.Value).
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("create custom field value: %w", err)
	}
	return result, nil
}

// GetValues retrieves all custom field values for an entity.
func (s *Service) GetValues(ctx context.Context, tenantID uuid.UUID, entityType string, entityID uuid.UUID) ([]*ent.CustomFieldValue, error) {
	results, err := s.client.CustomFieldValue.Query().
		Where(
			customfieldvalue.TenantID(tenantID),
			customfieldvalue.EntityTypeEQ(entityType),
			customfieldvalue.EntityID(entityID),
		).
		WithFieldDef().
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("get custom field values: %w", err)
	}
	return results, nil
}
