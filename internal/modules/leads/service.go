package leads

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/platform/adconversions"
	"github.com/bengobox/marketflow-api/internal/platform/search"
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
	// ID, when set, is the lead's fixed id (MetaLeadID): a second create with it returns
	// ErrDuplicateLead, so racing webhook deliveries make one lead.
	ID                   *uuid.UUID
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
	// Status and CreatedAt are set only by Import, which keeps a migrated lead's state and age.
	Status    string
	CreatedAt *time.Time
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
	// Sources and ExcludeSources filter on several sources at once (a website's enquiry forms,
	// or every lead but those).
	Sources        []string
	ExcludeSources []string
	// Statuses filters on several statuses at once (every open lead: all but lost).
	Statuses []string
	LeadType *string
	// Search matches name, email, phone or interest (case-insensitive substring), at most 100
	// characters.
	Search string
	From   *time.Time
	To     *time.Time
	Limit  int
	Offset int
}

// Create saves a lead submission through Capture and returns the lead: a new one (with
// lead.created in the same transaction) or, for a person the tenant already has, their existing
// lead with the submission recorded on it. Every lead source (public funnel, availability
// booking, Meta and TikTok webhooks, API) goes through here, so nurture auto-enrolment and lead
// scoring see every person exactly once.
func (s *Service) Create(ctx context.Context, in CreateInput) (*ent.Lead, error) {
	res, err := s.Capture(ctx, in)
	if err != nil {
		return nil, err
	}
	return res.Lead, nil
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
	if len(in.Statuses) > 0 {
		query = query.Where(lead.StatusIn(in.Statuses...))
	}
	if len(in.Sources) > 0 {
		query = query.Where(lead.SourceIn(in.Sources...))
	}
	if len(in.ExcludeSources) > 0 {
		query = query.Where(lead.SourceNotIn(in.ExcludeSources...))
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
	// Every word must match a name, email or phone, so a full name typed as "Jane Doe" finds the
	// lead stored as first name Jane, last name Doe (same rule as the contacts search).
	for _, t := range search.Terms(in.Search) {
		query = query.Where(lead.Or(
			lead.FirstNameContainsFold(t),
			lead.LastNameContainsFold(t),
			lead.EmailContainsFold(t),
			lead.PhoneContains(t),
			lead.InterestContainsFold(t),
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
		builder.SetMetadata(mergeMetadata(existing.Metadata, in.Metadata))
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
	// A lead that came from an ad and is now qualified or converted is reported to the tenant's own
	// ad accounts, once (owner, 2026-11-25). The save stands even when this cannot be queued.
	if in.Status != nil && *in.Status != existing.Status && (*in.Status == "qualified" || *in.Status == "converted") {
		if _, err := adconversions.QualifiedLead(ctx, s.client, result); err != nil {
			s.logger.Warn("lead conversion not queued", zap.String("lead", result.ID.String()), zap.Error(err))
		}
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

// ErrDuplicateLead: a lead with this fixed id already exists (a redelivered webhook).
var ErrDuplicateLead = errors.New("lead already exists")

// metaLeadNamespace scopes MetaLeadID's name-based UUIDs.
var metaLeadNamespace = uuid.MustParse("6f1e0b3a-6b0e-4c55-9a2f-3d5b7c1e8a90")

// MetaLeadID is the fixed id of the lead for one Meta leadgen id in one tenant (UUID v5), so a
// redelivered or racing webhook cannot create a second lead.
func MetaLeadID(tenantID uuid.UUID, leadgenID string) uuid.UUID {
	return uuid.NewSHA1(metaLeadNamespace, []byte(tenantID.String()+"/"+leadgenID))
}

// mergeMetadata applies a metadata patch to a lead's metadata: keys in the patch replace or add,
// a key set to null is removed, and every other key stays. Before this an update replaced the
// whole map, so editing a note wiped the consent record and the ad attribution.
func mergeMetadata(current, patch map[string]any) map[string]any {
	out := make(map[string]any, len(current)+len(patch))
	for k, v := range current {
		out[k] = v
	}
	for k, v := range patch {
		if v == nil {
			delete(out, k)
			continue
		}
		out[k] = v
	}
	return out
}
