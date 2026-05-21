package knowledge

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"

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
// Returns both the tenant's private rows AND the platform-shared rows
// (scope='platform') so a fresh tenant still sees the seeded catalog.
func (s *Service) ListEntries(ctx context.Context, in ListEntriesInput) ([]*ent.KnowledgeBaseEntry, int, error) {
	query := s.client.KnowledgeBaseEntry.Query().
		Where(knowledgebaseentry.Or(
			knowledgebaseentry.TenantID(in.TenantID),
			knowledgebaseentry.ScopeEQ("platform"),
		))

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

// BulkImportResult holds the outcome of a CSV import.
type BulkImportResult struct {
	Imported int      `json:"imported"`
	Skipped  int      `json:"skipped"`
	Errors   []string `json:"errors,omitempty"`
}

// BulkImportCSV parses a CSV reader and bulk-creates KB entries for the tenant.
// Expected columns (header row required):
//
//	category, title, content, product_code, product_price, product_cc,
//	product_image_url, tags
//
// Only title and content are mandatory; all other columns are optional.
func (s *Service) BulkImportCSV(ctx context.Context, tenantID uuid.UUID, r io.Reader) (BulkImportResult, error) {
	reader := csv.NewReader(r)
	reader.TrimLeadingSpace = true

	header, err := reader.Read()
	if err != nil {
		return BulkImportResult{}, fmt.Errorf("read CSV header: %w", err)
	}
	col := make(map[string]int, len(header))
	for i, h := range header {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	if _, ok := col["title"]; !ok {
		return BulkImportResult{}, fmt.Errorf("CSV missing required 'title' column")
	}
	if _, ok := col["content"]; !ok {
		return BulkImportResult{}, fmt.Errorf("CSV missing required 'content' column")
	}

	get := func(row []string, name string) string {
		i, ok := col[name]
		if !ok || i >= len(row) {
			return ""
		}
		return strings.TrimSpace(row[i])
	}
	getPtr := func(row []string, name string) *string {
		v := get(row, name)
		if v == "" {
			return nil
		}
		return &v
	}
	getFloat := func(row []string, name string) *float64 {
		v := get(row, name)
		if v == "" {
			return nil
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil
		}
		return &f
	}

	var result BulkImportResult
	lineNum := 1
	for {
		row, err := reader.Read()
		lineNum++
		if err == io.EOF {
			break
		}
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("line %d: %s", lineNum, err))
			result.Skipped++
			continue
		}

		title := get(row, "title")
		content := get(row, "content")
		if title == "" || content == "" {
			result.Errors = append(result.Errors, fmt.Sprintf("line %d: title and content are required", lineNum))
			result.Skipped++
			continue
		}

		category := get(row, "category")
		if category == "" {
			category = "product"
		}

		var tags []string
		if t := get(row, "tags"); t != "" {
			for _, tag := range strings.Split(t, "|") {
				if v := strings.TrimSpace(tag); v != "" {
					tags = append(tags, v)
				}
			}
		}

		in := CreateEntryInput{
			TenantID:        tenantID,
			Category:        category,
			Title:           title,
			Content:         content,
			ProductCode:     getPtr(row, "product_code"),
			ProductPrice:    getFloat(row, "product_price"),
			ProductCC:       getFloat(row, "product_cc"),
			ProductImageURL: getPtr(row, "product_image_url"),
			Tags:            tags,
		}

		if _, err := s.CreateEntry(ctx, in); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("line %d: %s", lineNum, err))
			result.Skipped++
			continue
		}
		result.Imported++
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
