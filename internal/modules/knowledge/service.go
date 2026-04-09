package knowledge

import (
	"context"
	"fmt"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/knowledgebaseentry"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Service handles knowledge base business logic.
type Service struct {
	client *ent.Client
	logger *zap.Logger
}

// NewService creates a new knowledge base service.
func NewService(client *ent.Client, logger *zap.Logger) *Service {
	return &Service{
		client: client,
		logger: logger.Named("knowledge"),
	}
}

// CreateEntryInput contains the data needed to create a KB entry.
type CreateEntryInput struct {
	TenantID        uuid.UUID
	Category        string
	Title           string
	Content         string
	ProductCode     *string
	ProductPrice    *float64
	ProductCC       *float64
	ProductImageURL *string
	Tags            []string
	Metadata        map[string]any
}

// UpdateEntryInput contains the data to update a KB entry.
type UpdateEntryInput struct {
	Category        *string
	Title           *string
	Content         *string
	ProductCode     *string
	ProductPrice    *float64
	ProductCC       *float64
	ProductImageURL *string
	Tags            []string
	Metadata        map[string]any
}

// ListEntriesInput contains filtering/pagination for listing KB entries.
type ListEntriesInput struct {
	TenantID uuid.UUID
	Category *string
	Embedded *bool
	Limit    int
	Offset   int
}

// CreateEntry creates a new knowledge base entry.
func (s *Service) CreateEntry(ctx context.Context, in CreateEntryInput) (*ent.KnowledgeBaseEntry, error) {
	builder := s.client.KnowledgeBaseEntry.Create().
		SetTenantID(in.TenantID).
		SetCategory(in.Category).
		SetTitle(in.Title).
		SetContent(in.Content)

	if in.ProductCode != nil {
		builder.SetNillableProductCode(in.ProductCode)
	}
	if in.ProductPrice != nil {
		builder.SetNillableProductPrice(in.ProductPrice)
	}
	if in.ProductCC != nil {
		builder.SetNillableProductCc(in.ProductCC)
	}
	if in.ProductImageURL != nil {
		builder.SetNillableProductImageURL(in.ProductImageURL)
	}
	if in.Tags != nil {
		builder.SetTags(in.Tags)
	}
	if in.Metadata != nil {
		builder.SetMetadata(in.Metadata)
	}

	result, err := builder.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("create KB entry: %w", err)
	}

	s.logger.Info("KB entry created",
		zap.String("id", result.ID.String()),
		zap.String("category", in.Category),
		zap.String("title", in.Title),
	)

	return result, nil
}

// GetEntry retrieves a KB entry by ID within a tenant.
func (s *Service) GetEntry(ctx context.Context, tenantID, entryID uuid.UUID) (*ent.KnowledgeBaseEntry, error) {
	result, err := s.client.KnowledgeBaseEntry.Query().
		Where(
			knowledgebaseentry.ID(entryID),
			knowledgebaseentry.TenantID(tenantID),
		).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("get KB entry: %w", err)
	}
	return result, nil
}

// ListEntries retrieves KB entries with filtering and pagination.
func (s *Service) ListEntries(ctx context.Context, in ListEntriesInput) ([]*ent.KnowledgeBaseEntry, int, error) {
	query := s.client.KnowledgeBaseEntry.Query().
		Where(knowledgebaseentry.TenantID(in.TenantID))

	if in.Category != nil {
		query = query.Where(knowledgebaseentry.CategoryEQ(*in.Category))
	}
	if in.Embedded != nil {
		query = query.Where(knowledgebaseentry.EmbeddedEQ(*in.Embedded))
	}

	total, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("count KB entries: %w", err)
	}

	if in.Limit <= 0 {
		in.Limit = 20
	}
	if in.Limit > 100 {
		in.Limit = 100
	}

	results, err := query.
		Order(ent.Desc(knowledgebaseentry.FieldCreatedAt)).
		Limit(in.Limit).
		Offset(in.Offset).
		All(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("list KB entries: %w", err)
	}

	return results, total, nil
}

// UpdateEntry updates a KB entry and marks it as needing re-embedding.
func (s *Service) UpdateEntry(ctx context.Context, tenantID, entryID uuid.UUID, in UpdateEntryInput) (*ent.KnowledgeBaseEntry, error) {
	existing, err := s.GetEntry(ctx, tenantID, entryID)
	if err != nil {
		return nil, err
	}

	builder := existing.Update()

	if in.Category != nil {
		builder.SetCategory(*in.Category)
	}
	if in.Title != nil {
		builder.SetTitle(*in.Title)
	}
	if in.Content != nil {
		builder.SetContent(*in.Content)
		// Content changed — mark as needing re-embedding
		builder.SetEmbedded(false).SetChunkCount(0)
	}
	if in.ProductCode != nil {
		builder.SetNillableProductCode(in.ProductCode)
	}
	if in.ProductPrice != nil {
		builder.SetNillableProductPrice(in.ProductPrice)
	}
	if in.ProductCC != nil {
		builder.SetNillableProductCc(in.ProductCC)
	}
	if in.ProductImageURL != nil {
		builder.SetNillableProductImageURL(in.ProductImageURL)
	}
	if in.Tags != nil {
		builder.SetTags(in.Tags)
	}
	if in.Metadata != nil {
		builder.SetMetadata(in.Metadata)
	}

	result, err := builder.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("update KB entry: %w", err)
	}

	return result, nil
}

// DeleteEntry removes a KB entry.
func (s *Service) DeleteEntry(ctx context.Context, tenantID, entryID uuid.UUID) error {
	_, err := s.client.KnowledgeBaseEntry.Delete().
		Where(
			knowledgebaseentry.ID(entryID),
			knowledgebaseentry.TenantID(tenantID),
		).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("delete KB entry: %w", err)
	}
	return nil
}
