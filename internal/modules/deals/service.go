package deals

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/account"
	"github.com/bengobox/marketflow-api/internal/ent/contact"
	"github.com/bengobox/marketflow-api/internal/ent/deal"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/ent/pipeline"
	"github.com/bengobox/marketflow-api/internal/ent/pipelinestage"
	"github.com/bengobox/marketflow-api/internal/outbox"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Service handles deal business logic.
type Service struct {
	client *ent.Client
	logger *zap.Logger
}

// NewService creates a new deal service.
func NewService(client *ent.Client, logger *zap.Logger) *Service {
	return &Service{
		client: client,
		logger: logger.Named("deals"),
	}
}

// CreateInput contains data needed to create a deal.
type CreateInput struct {
	TenantID          uuid.UUID
	Title             string
	ContactID         *uuid.UUID
	LeadID            *uuid.UUID
	AccountID         *uuid.UUID
	PipelineID        uuid.UUID
	StageID           uuid.UUID
	Value             *float64
	Currency          string
	Probability       *float64
	ExpectedCloseDate *time.Time
	OwnerUserID       *uuid.UUID
	Metadata          map[string]any
}

// UpdateInput contains data to update a deal.
type UpdateInput struct {
	Title             *string
	ContactID         *uuid.UUID
	AccountID         *uuid.UUID
	StageID           *uuid.UUID
	Value             *float64
	Currency          *string
	Probability       *float64
	ExpectedCloseDate *time.Time
	OwnerUserID       *uuid.UUID
	Metadata          map[string]any
}

// ListInput contains filtering and pagination for listing deals.
type ListInput struct {
	TenantID   uuid.UUID
	Status     *string
	PipelineID *uuid.UUID
	StageID    *uuid.UUID
	ContactID  *uuid.UUID
	LeadID     *uuid.UUID
	AccountID  *uuid.UUID
	OwnerID    *uuid.UUID
	Limit      int
	Offset     int
}

// ErrNotInTenant is a pipeline, stage, contact, lead or account id that is not the tenant's, or a
// stage outside the deal's pipeline. Handlers answer it 404.
var ErrNotInTenant = errors.New("not found")

// refs are the records a deal points at; nil ones are not checked.
type refs struct {
	pipelineID, stageID, contactID, leadID, accountID *uuid.UUID
}

// checkRefs makes sure every id names a record of the tenant, and the stage one of the pipeline.
func (s *Service) checkRefs(ctx context.Context, tenantID uuid.UUID, r refs) error {
	checks := []struct {
		id    *uuid.UUID
		exist func(uuid.UUID) (bool, error)
	}{
		{r.pipelineID, func(id uuid.UUID) (bool, error) {
			return s.client.Pipeline.Query().Where(pipeline.ID(id), pipeline.TenantID(tenantID)).Exist(ctx)
		}},
		{r.stageID, func(id uuid.UUID) (bool, error) {
			q := s.client.PipelineStage.Query().Where(pipelinestage.ID(id), pipelinestage.TenantID(tenantID))
			if r.pipelineID != nil {
				q = q.Where(pipelinestage.PipelineID(*r.pipelineID))
			}
			return q.Exist(ctx)
		}},
		{r.contactID, func(id uuid.UUID) (bool, error) {
			return s.client.Contact.Query().Where(contact.ID(id), contact.TenantID(tenantID)).Exist(ctx)
		}},
		{r.leadID, func(id uuid.UUID) (bool, error) {
			return s.client.Lead.Query().Where(lead.ID(id), lead.TenantID(tenantID)).Exist(ctx)
		}},
		{r.accountID, func(id uuid.UUID) (bool, error) {
			return s.client.Account.Query().Where(account.ID(id), account.TenantID(tenantID)).Exist(ctx)
		}},
	}
	for _, c := range checks {
		if c.id == nil {
			continue
		}
		ok, err := c.exist(*c.id)
		if err != nil {
			return fmt.Errorf("check deal references: %w", err)
		}
		if !ok {
			return ErrNotInTenant
		}
	}
	return nil
}

// Create creates a new deal.
func (s *Service) Create(ctx context.Context, in CreateInput) (*ent.Deal, error) {
	if err := s.checkRefs(ctx, in.TenantID, refs{pipelineID: &in.PipelineID, stageID: &in.StageID,
		contactID: in.ContactID, leadID: in.LeadID, accountID: in.AccountID}); err != nil {
		return nil, err
	}
	currency := in.Currency
	if currency == "" {
		currency = "USD"
	}

	builder := s.client.Deal.Create().
		SetTenantID(in.TenantID).
		SetTitle(in.Title).
		SetPipelineID(in.PipelineID).
		SetStageID(in.StageID).
		SetCurrency(currency)

	if in.ContactID != nil {
		builder.SetNillableContactID(in.ContactID)
	}
	if in.LeadID != nil {
		builder.SetNillableLeadID(in.LeadID)
	}
	if in.AccountID != nil {
		builder.SetNillableAccountID(in.AccountID)
	}
	if in.Value != nil {
		builder.SetNillableValue(in.Value)
	}
	if in.Probability != nil {
		builder.SetNillableProbability(in.Probability)
	}
	if in.ExpectedCloseDate != nil {
		builder.SetNillableExpectedCloseDate(in.ExpectedCloseDate)
	}
	if in.OwnerUserID != nil {
		builder.SetNillableOwnerUserID(in.OwnerUserID)
	}
	if in.Metadata != nil {
		builder.SetMetadata(in.Metadata)
	}

	result, err := builder.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("create deal: %w", err)
	}

	s.logger.Info("deal created",
		zap.String("id", result.ID.String()),
		zap.String("tenant_id", in.TenantID.String()),
		zap.String("title", in.Title),
	)

	return result, nil
}

// Get retrieves a deal by ID within a tenant.
func (s *Service) Get(ctx context.Context, tenantID, dealID uuid.UUID) (*ent.Deal, error) {
	result, err := s.client.Deal.Query().
		Where(deal.ID(dealID), deal.TenantID(tenantID)).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("get deal: %w", err)
	}
	return result, nil
}

// List retrieves deals with filtering and pagination.
func (s *Service) List(ctx context.Context, in ListInput) ([]*ent.Deal, int, error) {
	query := s.client.Deal.Query().Where(deal.TenantID(in.TenantID))

	if in.Status != nil {
		query = query.Where(deal.StatusEQ(*in.Status))
	}
	if in.PipelineID != nil {
		query = query.Where(deal.PipelineID(*in.PipelineID))
	}
	if in.StageID != nil {
		query = query.Where(deal.StageID(*in.StageID))
	}
	if in.ContactID != nil {
		query = query.Where(deal.ContactID(*in.ContactID))
	}
	if in.LeadID != nil {
		query = query.Where(deal.LeadID(*in.LeadID))
	}
	if in.AccountID != nil {
		query = query.Where(deal.AccountID(*in.AccountID))
	}
	if in.OwnerID != nil {
		query = query.Where(deal.OwnerUserID(*in.OwnerID))
	}

	total, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("count deals: %w", err)
	}

	if in.Limit <= 0 {
		in.Limit = 20
	}
	if in.Limit > 100 {
		in.Limit = 100
	}

	results, err := query.
		Order(ent.Desc(deal.FieldCreatedAt)).
		Limit(in.Limit).
		Offset(in.Offset).
		All(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("list deals: %w", err)
	}

	return results, total, nil
}

// Update updates deal fields.
func (s *Service) Update(ctx context.Context, tenantID, dealID uuid.UUID, in UpdateInput) (*ent.Deal, error) {
	existing, err := s.Get(ctx, tenantID, dealID)
	if err != nil {
		return nil, err
	}
	// A new stage must be in the deal's own pipeline.
	check := refs{stageID: in.StageID, contactID: in.ContactID, accountID: in.AccountID}
	if in.StageID != nil {
		check.pipelineID = &existing.PipelineID
	}
	if err := s.checkRefs(ctx, tenantID, check); err != nil {
		return nil, err
	}

	stageChanged := in.StageID != nil && *in.StageID != existing.StageID
	if stageChanged {
		return s.saveStageMove(ctx, existing, func(b *ent.DealUpdateOne) { applyUpdate(b, in) })
	}
	builder := existing.Update()
	applyUpdate(builder, in)
	result, err := builder.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("update deal: %w", err)
	}
	return result, nil
}

// saveStageMove saves a change that moves the deal to another stage together
// with its crm.deal.stage_changed event, so automations never miss a move.
func (s *Service) saveStageMove(ctx context.Context, existing *ent.Deal, apply func(*ent.DealUpdateOne)) (*ent.Deal, error) {
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, fmt.Errorf("move deal stage: begin tx: %w", err)
	}
	b := tx.Deal.UpdateOneID(existing.ID)
	apply(b)
	result, err := b.Save(ctx)
	if err == nil {
		err = outbox.Enqueue(ctx, tx.OutboxEvent, dealStageChangedEvent(result, existing.StageID))
	}
	if err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("move deal stage: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("move deal stage: commit: %w", err)
	}
	return result, nil
}

func applyUpdate(builder *ent.DealUpdateOne, in UpdateInput) {
	if in.Title != nil {
		builder.SetTitle(*in.Title)
	}
	if in.ContactID != nil {
		builder.SetNillableContactID(in.ContactID)
	}
	if in.AccountID != nil {
		builder.SetNillableAccountID(in.AccountID)
	}
	if in.StageID != nil {
		builder.SetStageID(*in.StageID)
	}
	if in.Value != nil {
		builder.SetNillableValue(in.Value)
	}
	if in.Currency != nil {
		builder.SetCurrency(*in.Currency)
	}
	if in.Probability != nil {
		builder.SetNillableProbability(in.Probability)
	}
	if in.ExpectedCloseDate != nil {
		builder.SetNillableExpectedCloseDate(in.ExpectedCloseDate)
	}
	if in.OwnerUserID != nil {
		builder.SetNillableOwnerUserID(in.OwnerUserID)
	}
	if in.Metadata != nil {
		builder.SetMetadata(in.Metadata)
	}
}

// Move moves a deal to a new pipeline stage.
func (s *Service) Move(ctx context.Context, tenantID, dealID, stageID uuid.UUID) (*ent.Deal, error) {
	existing, err := s.Get(ctx, tenantID, dealID)
	if err != nil {
		return nil, err
	}
	if existing.StageID == stageID {
		return existing, nil // already there: no move, no event
	}
	if err := s.checkRefs(ctx, tenantID, refs{pipelineID: &existing.PipelineID, stageID: &stageID}); err != nil {
		return nil, err
	}
	result, err := s.saveStageMove(ctx, existing, func(b *ent.DealUpdateOne) { b.SetStageID(stageID) })
	if err != nil {
		return nil, err
	}
	s.logger.Info("deal moved",
		zap.String("deal_id", dealID.String()),
		zap.String("stage_id", stageID.String()),
	)
	return result, nil
}

// MarkWon marks a deal as won.
func (s *Service) MarkWon(ctx context.Context, tenantID, dealID uuid.UUID) (*ent.Deal, error) {
	existing, err := s.Get(ctx, tenantID, dealID)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	result, err := existing.Update().
		SetStatus("won").
		SetNillableWonAt(&now).
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("mark deal won: %w", err)
	}
	return result, nil
}

// MarkLost marks a deal as lost with an optional reason.
func (s *Service) MarkLost(ctx context.Context, tenantID, dealID uuid.UUID, reason *string) (*ent.Deal, error) {
	existing, err := s.Get(ctx, tenantID, dealID)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	builder := existing.Update().
		SetStatus("lost").
		SetNillableLostAt(&now)
	if reason != nil {
		builder.SetNillableLostReason(reason)
	}
	result, err := builder.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("mark deal lost: %w", err)
	}
	return result, nil
}

// Delete hard-deletes a deal.
func (s *Service) Delete(ctx context.Context, tenantID, dealID uuid.UUID) error {
	n, err := s.client.Deal.Delete().
		Where(deal.ID(dealID), deal.TenantID(tenantID)).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("delete deal: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("deal not found")
	}
	return nil
}
