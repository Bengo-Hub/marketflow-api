package leads

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/outbox"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Service handles lead business logic.
type Service struct {
	client *ent.Client
	logger *zap.Logger
}

// NewService creates a new lead service.
func NewService(client *ent.Client, logger *zap.Logger) *Service {
	return &Service{
		client: client,
		logger: logger.Named("leads"),
	}
}

// CreateInput contains the data needed to create a lead.
type CreateInput struct {
	TenantID             uuid.UUID
	Email                *string
	Phone                *string
	FirstName            *string
	LastName             *string
	Source               string
	CampaignID           *uuid.UUID
	FunnelID             *uuid.UUID
	UTMSource            *string
	UTMMedium            *string
	UTMCampaign          *string
	LeadType             string
	Interest             *string
	QualificationAnswers map[string]any
	Metadata             map[string]any
}

// UpdateInput contains the data to update a lead.
type UpdateInput struct {
	Email                *string
	Phone                *string
	FirstName            *string
	LastName             *string
	Status               *string
	Score                *int
	Interest             *string
	QualificationAnswers map[string]any
	Metadata             map[string]any
}

// ListInput contains filtering/pagination for listing leads.
type ListInput struct {
	TenantID uuid.UUID
	Status   *string
	Source   *string
	LeadType *string
	// Search matches name, email or phone (case-insensitive substring), at most 100 characters.
	Search string
	From   *time.Time
	To     *time.Time
	Limit  int
	Offset int
}

// Create creates a new lead and, in the same transaction, the outbox row for its lead.created
// event. Every lead source (public funnel, availability booking, Meta and TikTok webhooks, API)
// goes through here, so nurture auto-enrolment and lead scoring see every lead exactly once.
func (s *Service) Create(ctx context.Context, in CreateInput) (*ent.Lead, error) {
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, fmt.Errorf("create lead: begin tx: %w", err)
	}
	rollback := func(cause error) (*ent.Lead, error) {
		_ = tx.Rollback()
		return nil, cause
	}

	builder := tx.Lead.Create().
		SetTenantID(in.TenantID).
		SetSource(in.Source).
		SetLeadType(in.LeadType)

	if in.Email != nil {
		builder.SetNillableEmail(in.Email)
	}
	if in.Phone != nil {
		builder.SetNillablePhone(in.Phone)
	}
	if in.FirstName != nil {
		builder.SetNillableFirstName(in.FirstName)
	}
	if in.LastName != nil {
		builder.SetNillableLastName(in.LastName)
	}
	if in.CampaignID != nil {
		builder.SetNillableCampaignID(in.CampaignID)
	}
	if in.FunnelID != nil {
		builder.SetNillableFunnelID(in.FunnelID)
	}
	if in.UTMSource != nil {
		builder.SetNillableUtmSource(in.UTMSource)
	}
	if in.UTMMedium != nil {
		builder.SetNillableUtmMedium(in.UTMMedium)
	}
	if in.UTMCampaign != nil {
		builder.SetNillableUtmCampaign(in.UTMCampaign)
	}
	if in.Interest != nil {
		builder.SetNillableInterest(in.Interest)
	}
	if in.QualificationAnswers != nil {
		builder.SetQualificationAnswers(in.QualificationAnswers)
	}
	if in.Metadata != nil {
		builder.SetMetadata(in.Metadata)
	}

	result, err := builder.Save(ctx)
	if err != nil {
		return rollback(fmt.Errorf("create lead: %w", err))
	}

	// In the transaction: a lead without its event would never be nurtured or scored.
	if err := outbox.Enqueue(ctx, tx.OutboxEvent, leadCreatedEvent(result)); err != nil {
		return rollback(err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("create lead: commit: %w", err)
	}

	s.logger.Info("lead created",
		zap.String("id", result.ID.String()),
		zap.String("tenant_id", in.TenantID.String()),
		zap.String("source", in.Source),
	)

	return result, nil
}

// Get retrieves a lead by ID within a tenant.
func (s *Service) Get(ctx context.Context, tenantID, leadID uuid.UUID) (*ent.Lead, error) {
	result, err := s.client.Lead.Query().
		Where(
			lead.ID(leadID),
			lead.TenantID(tenantID),
		).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("get lead: %w", err)
	}
	return result, nil
}

// List retrieves leads with filtering and pagination.
func (s *Service) List(ctx context.Context, in ListInput) ([]*ent.Lead, int, error) {
	query := s.filtered(in)

	total, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("count leads: %w", err)
	}

	if in.Limit <= 0 {
		in.Limit = 20
	}
	if in.Limit > 100 {
		in.Limit = 100
	}

	results, err := query.
		Order(ent.Desc(lead.FieldCreatedAt)).
		Limit(in.Limit).
		Offset(in.Offset).
		All(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("list leads: %w", err)
	}

	return results, total, nil
}

// exportPage is how many leads one export query reads (a variable so tests can
// cross page boundaries with a few rows).
var exportPage = 1000

// ExportEach streams every lead matching the filter to fn, newest first, in
// keyset pages on (created_at, id) over leads(tenant_id, created_at). Memory
// stays at one page however many leads a tenant has.
func (s *Service) ExportEach(ctx context.Context, in ListInput, fn func(*ent.Lead) error) error {
	var cursor *ent.Lead
	for {
		query := s.exportQuery(in)
		if cursor != nil {
			c := cursor
			query = query.Where(lead.Or(
				lead.CreatedAtLT(c.CreatedAt),
				lead.And(lead.CreatedAtEQ(c.CreatedAt), lead.IDLT(c.ID)),
			))
		}
		page, err := query.Order(ent.Desc(lead.FieldCreatedAt), ent.Desc(lead.FieldID)).Limit(exportPage).All(ctx)
		if err != nil {
			return fmt.Errorf("export leads: %w", err)
		}
		for _, l := range page {
			if err := fn(l); err != nil {
				return err
			}
		}
		if len(page) < exportPage {
			return nil
		}
		cursor = page[len(page)-1]
	}
}

func (s *Service) exportQuery(in ListInput) *ent.LeadQuery {
	return s.filtered(in)
}

// filtered is the one filter set shared by the list and the CSV export, so the export always
// matches what the list shows.
func (s *Service) filtered(in ListInput) *ent.LeadQuery {
	query := s.client.Lead.Query().
		Where(lead.TenantID(in.TenantID))

	if in.Status != nil {
		query = query.Where(lead.StatusEQ(*in.Status))
	}
	if in.Source != nil {
		query = query.Where(lead.SourceEQ(*in.Source))
	}
	if in.LeadType != nil {
		query = query.Where(lead.LeadTypeEQ(*in.LeadType))
	}
	if in.From != nil {
		query = query.Where(lead.CreatedAtGTE(*in.From))
	}
	if in.To != nil {
		query = query.Where(lead.CreatedAtLTE(*in.To))
	}
	if q := strings.TrimSpace(in.Search); q != "" {
		if len(q) > 100 {
			q = q[:100]
		}
		query = query.Where(lead.Or(
			lead.FirstNameContainsFold(q),
			lead.LastNameContainsFold(q),
			lead.EmailContainsFold(q),
			lead.PhoneContains(q),
		))
	}
	return query
}

// Update updates a lead.
func (s *Service) Update(ctx context.Context, tenantID, leadID uuid.UUID, in UpdateInput) (*ent.Lead, error) {
	existing, err := s.Get(ctx, tenantID, leadID)
	if err != nil {
		return nil, err
	}

	builder := existing.Update()

	if in.Email != nil {
		builder.SetNillableEmail(in.Email)
	}
	if in.Phone != nil {
		builder.SetNillablePhone(in.Phone)
	}
	if in.FirstName != nil {
		builder.SetNillableFirstName(in.FirstName)
	}
	if in.LastName != nil {
		builder.SetNillableLastName(in.LastName)
	}
	if in.Score != nil {
		builder.SetScore(*in.Score)
	}
	if in.Interest != nil {
		builder.SetNillableInterest(in.Interest)
	}
	if in.QualificationAnswers != nil {
		builder.SetQualificationAnswers(in.QualificationAnswers)
	}
	if in.Metadata != nil {
		builder.SetMetadata(in.Metadata)
	}

	// Handle status transitions with timestamp tracking
	if in.Status != nil && *in.Status != existing.Status {
		builder.SetStatus(*in.Status)
		now := time.Now()
		switch *in.Status {
		case "contacted":
			builder.SetContactedAt(now)
		case "qualified":
			builder.SetQualifiedAt(now)
		case "converted":
			builder.SetConvertedAt(now)
		}
	}

	result, err := builder.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("update lead: %w", err)
	}

	return result, nil
}

// Delete soft-deletes a lead by marking it as lost.
func (s *Service) Delete(ctx context.Context, tenantID, leadID uuid.UUID) error {
	_, err := s.client.Lead.Update().
		Where(
			lead.ID(leadID),
			lead.TenantID(tenantID),
		).
		SetStatus("lost").
		Save(ctx)
	if err != nil {
		return fmt.Errorf("delete lead: %w", err)
	}
	return nil
}
