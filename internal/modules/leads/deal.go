package leads

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqljson"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	entcontact "github.com/bengobox/marketflow-api/internal/ent/contact"
	"github.com/bengobox/marketflow-api/internal/ent/deal"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/ent/pipeline"
	"github.com/bengobox/marketflow-api/internal/ent/pipelinestage"
	"github.com/bengobox/marketflow-api/internal/modules/contacts"
	"github.com/bengobox/marketflow-api/internal/modules/deals"
	"github.com/bengobox/marketflow-api/internal/modules/pipelines"
	"github.com/bengobox/marketflow-api/internal/modules/tenant"
	"github.com/bengobox/marketflow-api/internal/platform/svcconfig"
)

// A qualified lead becomes one deal in the first stage of the tenant's sales pipeline (owner,
// 2026-11-25); so does a converted lead that has none.

const (
	// DealFromLeadKey is "off" to make no deals from leads; a tenant's row wins.
	DealFromLeadKey = "deals.from_qualified_leads"
	// DealRepairKey is how far back the worker looks for a lead whose deal was not made.
	DealRepairKey = "deals.from_qualified_leads_repair"
	// DealRepairDefault is DealRepairKey when the platform set nothing.
	DealRepairDefault = 7 * 24 * time.Hour

	// metaDealID is the lead's deal, kept so a deal somebody deleted is not made again.
	metaDealID   = "deal_id"
	customerType = "customer"
	repairPage   = 200
)

var errNoStage = errors.New("the sales pipeline has no open stage")

// EnsureDeal makes the lead's deal when it should have one and has none. It is safe to call again
// and at the same time; it returns the deal it made, or nil.
func (s *Service) EnsureDeal(ctx context.Context, tenantID, leadID uuid.UUID) (*ent.Deal, error) {
	l, err := s.Get(ctx, tenantID, leadID)
	if err != nil {
		return nil, err
	}
	if !wantsDeal(l) || strings.EqualFold(svcconfig.Value(ctx, s.client, tenantID, DealFromLeadKey), "off") {
		return nil, nil
	}
	t, err := s.client.Tenant.Get(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("lead deal: %w", err)
	}
	// A job or attachment applicant or a service pitch entered by hand is not a customer either.
	if PlatformLeadFilter().WithTenant(t.Metadata).NonLeadKind(CreateInput{Interest: l.Interest,
		QualificationAnswers: l.QualificationAnswers, Metadata: l.Metadata}) != "" {
		return nil, nil
	}
	people, err := s.leadContacts(ctx, l)
	if err != nil {
		return nil, fmt.Errorf("lead deal: contacts: %w", err)
	}
	if contacts.OptedOut(people, l.Metadata) {
		return nil, nil
	}
	if _, err := pipelines.NewService(s.client, s.logger).EnsureDefault(ctx, tenantID); err != nil {
		return nil, err
	}

	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, fmt.Errorf("lead deal: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// Two calls at once make one deal.
	if _, err := tx.ExecContext(ctx, "SELECT 1 FROM leads WHERE id = $1 FOR UPDATE", leadID); err != nil {
		return nil, fmt.Errorf("lead deal: lock: %w", err)
	}
	if l, err = tx.Lead.Get(ctx, leadID); err != nil {
		return nil, fmt.Errorf("lead deal: %w", err)
	}
	if !wantsDeal(l) {
		return nil, nil
	}
	if has, err := tx.Deal.Query().Where(deal.TenantID(tenantID), deal.LeadID(leadID)).Exist(ctx); err != nil || has {
		return nil, err
	}
	p, err := tx.Pipeline.Query().Where(pipeline.TenantID(tenantID)).
		Order(ent.Desc(pipeline.FieldIsDefault), ent.Asc(pipeline.FieldCreatedAt), ent.Asc(pipeline.FieldID)).First(ctx)
	if err != nil {
		return nil, fmt.Errorf("lead deal: pipeline: %w", err)
	}
	st, err := tx.PipelineStage.Query().
		Where(pipelinestage.TenantID(tenantID), pipelinestage.PipelineID(p.ID), pipelinestage.IsWonStage(false), pipelinestage.IsLostStage(false)).
		Order(ent.Asc(pipelinestage.FieldStageOrder), ent.Asc(pipelinestage.FieldID)).First(ctx)
	if ent.IsNotFound(err) {
		return nil, errNoStage
	}
	if err != nil {
		return nil, fmt.Errorf("lead deal: stage: %w", err)
	}
	// No value: a lead records none, and an amount is never made up.
	in := deals.CreateInput{TenantID: tenantID, Title: dealTitle(l), LeadID: &leadID, PipelineID: p.ID, StageID: st.ID,
		OwnerUserID: l.OwnerUserID, Metadata: map[string]any{"source": "lead"}}
	in.Currency, _ = tenant.ResolveCurrency(t.Metadata, t.Country, tenant.PlatformCurrency)
	if len(people) > 0 {
		in.ContactID, in.AccountID = &people[0].ID, people[0].AccountID
		if in.OwnerUserID == nil {
			in.OwnerUserID = people[0].OwnerUserID
		}
	}
	d, err := deals.NewService(tx.Client(), s.logger).Create(ctx, in)
	if err != nil {
		return nil, err
	}
	if err := tx.Lead.UpdateOneID(leadID).SetMetadata(mergeMetadata(l.Metadata, map[string]any{metaDealID: d.ID.String()})).Exec(ctx); err != nil {
		return nil, fmt.Errorf("lead deal: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("lead deal: commit: %w", err)
	}
	return d, nil
}

// RepairDeals makes the deals that were missed (the lead was saved, its deal was not). It returns
// how many it made.
func (s *Service) RepairDeals(ctx context.Context, now time.Time) (int, error) {
	back, ok := svcconfig.ParseDuration(svcconfig.Value(ctx, s.client, uuid.Nil, DealRepairKey), time.Hour, 90*24*time.Hour)
	if !ok {
		back = DealRepairDefault
	}
	return s.repairDeals(ctx, now.Add(-back))
}

func (s *Service) repairDeals(ctx context.Context, since time.Time, more ...func(*entsql.Selector)) (int, error) {
	made, after := 0, uuid.Nil
	for {
		q := s.client.Lead.Query().Where(
			lead.StatusIn("qualified", "converted"), lead.LeadTypeEQ(customerType), lead.IDGT(after),
			lead.Or(lead.QualifiedAtGTE(since), lead.ConvertedAtGTE(since)),
			func(sel *entsql.Selector) {
				sel.Where(entsql.Not(sqljson.HasKey(sel.C(lead.FieldMetadata), sqljson.Path(metaDealID))))
				d := entsql.Table(deal.Table)
				sel.Where(entsql.NotExists(entsql.Select(d.C(deal.FieldID)).From(d).Where(entsql.ColumnsEQ(d.C(deal.FieldLeadID), sel.C(lead.FieldID)))))
			})
		for _, m := range more {
			q = q.Where(m)
		}
		rows, err := q.Order(ent.Asc(lead.FieldID)).Limit(repairPage).All(ctx)
		if err != nil {
			return made, err
		}
		for _, l := range rows {
			d, err := s.EnsureDeal(ctx, l.TenantID, l.ID)
			if err != nil {
				s.logger.Error("lead deal not made", zap.String("lead", l.ID.String()), zap.Error(err))
				continue
			}
			if d != nil {
				made++
			}
		}
		if len(rows) < repairPage {
			return made, nil
		}
		after = rows[len(rows)-1].ID
	}
}

// wantsDeal: a qualified or converted customer lead that was never given a deal.
func wantsDeal(l *ent.Lead) bool {
	return (l.Status == "qualified" || l.Status == "converted") && l.LeadType == customerType && l.Metadata[metaDealID] == nil
}

// leadContacts are the lead's own contacts first, then those with its email or phone.
func (s *Service) leadContacts(ctx context.Context, l *ent.Lead) ([]*ent.Contact, error) {
	own, err := s.client.Contact.Query().Where(entcontact.TenantID(l.TenantID), entcontact.LeadID(l.ID)).
		Order(ent.Asc(entcontact.FieldCreatedAt)).Limit(20).All(ctx)
	if err != nil {
		return nil, err
	}
	email, phone := matchKeys(l.Email, l.Phone)
	others, err := contacts.PersonContacts(ctx, s.client, l.TenantID, email, phone)
	return append(own, others...), err
}

// dealTitle is the person's name, else how to reach them, with what they asked about when short.
func dealTitle(l *ent.Lead) string {
	title := strings.TrimSpace(derefStr(l.FirstName) + " " + derefStr(l.LastName))
	for _, v := range []*string{l.Email, l.Phone} {
		if title == "" {
			title = strings.TrimSpace(derefStr(v))
		}
	}
	if title == "" {
		title = "Lead"
	}
	if about := strings.TrimSpace(strings.ReplaceAll(derefStr(l.Interest), "_", " ")); about != "" && len([]rune(about)) <= 60 && !strings.Contains(about, "\n") {
		title += ", " + about
	}
	return title
}
