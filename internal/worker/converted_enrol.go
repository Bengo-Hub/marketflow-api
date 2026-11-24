package worker

import (
	"context"
	"time"

	sharedcache "github.com/Bengo-Hub/cache"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/ent/nurturesequence"
)

// ConvertedTrigger is the sequence trigger for a lead that became a customer. The seeded
// "Ask for a testimonial" template uses it: a week after the sale, the customer gets the tenant's
// public testimonial link ({{testimonial_link}}); what they send waits for the tenant's review.
const ConvertedTrigger = "lead.converted"

// convertedLookback bounds which conversions count: a tenant that turns the sequence on does not
// email every customer it ever converted, only the recent ones.
const convertedLookback = 30 * 24 * time.Hour

// ConvertedEnrol enrols each converted lead once into its tenant's active "lead.converted"
// sequence (the oldest one of the tenant's own). It runs hourly; the sequence's step delays decide
// when the message goes out. Opt-in: a tenant without such a sequence gets nothing.
type ConvertedEnrol struct {
	db     *ent.Client
	every  time.Duration
	logger *zap.Logger
}

func NewConvertedEnrol(db *ent.Client, logger *zap.Logger) *ConvertedEnrol {
	return &ConvertedEnrol{db: db, every: time.Hour, logger: logger.Named("converted-enrol")}
}

func (c *ConvertedEnrol) Run(ctx context.Context) {
	t := time.NewTicker(c.every)
	defer t.Stop()
	for {
		if sharedcache.ClaimPeriod(ctx, "marketflow:converted-enrol", c.every) {
			if n, err := c.enrol(ctx, time.Now().UTC()); err != nil {
				c.logger.Warn("converted enrol run failed", zap.Int("enrolled_before_error", n), zap.Error(err))
			} else if n > 0 {
				c.logger.Info("converted leads enrolled", zap.Int("count", n))
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// enrol does one run and returns how many enrolments it created.
func (c *ConvertedEnrol) enrol(ctx context.Context, now time.Time) (int, error) {
	seqs, err := c.db.NurtureSequence.Query().
		Where(
			nurturesequence.ScopeEQ("tenant"),
			nurturesequence.TriggerEventEQ(ConvertedTrigger),
			nurturesequence.ActiveEQ(true),
		).
		Order(ent.Asc(nurturesequence.FieldCreatedAt), ent.Asc(nurturesequence.FieldID)).
		All(ctx)
	if err != nil {
		return 0, err
	}
	seqByTenant := map[uuid.UUID]uuid.UUID{}
	firstDelay := map[uuid.UUID]time.Duration{} // step 0's delay per sequence
	for _, s := range seqs {
		if _, ok := seqByTenant[s.TenantID]; !ok {
			seqByTenant[s.TenantID] = s.ID
			if len(s.Steps) > 0 {
				if h := asInt(s.Steps[0], "delay_hours"); h > 0 {
					firstDelay[s.ID] = time.Duration(h) * time.Hour
				}
			}
		}
	}
	if len(seqByTenant) == 0 {
		return 0, nil
	}
	tenantIDs := make([]uuid.UUID, 0, len(seqByTenant))
	seqIDs := make([]uuid.UUID, 0, len(seqByTenant))
	for t, s := range seqByTenant {
		tenantIDs = append(tenantIDs, t)
		seqIDs = append(seqIDs, s)
	}
	enrolled := 0
	perSeq := map[uuid.UUID]int{}
	after := uuid.Nil
	for i := 0; i < reengagementMaxBatches; i++ {
		rows, err := c.db.Lead.Query().
			Where(
				lead.TenantIDIn(tenantIDs...),
				lead.StatusEQ("converted"),
				lead.ConvertedAtGTE(now.Add(-convertedLookback)),
				lead.IDGT(after),
				notEnrolledIn(seqIDs),
			).
			Order(ent.Asc(lead.FieldID)).
			Limit(reengagementBatch).
			All(ctx)
		if err != nil {
			return enrolled, err
		}
		for _, ld := range rows {
			seqID := seqByTenant[ld.TenantID]
			start := now
			if ld.ConvertedAt != nil {
				start = *ld.ConvertedAt // the first step's delay counts from the sale, not from this run
			}
			start = start.Add(firstDelay[seqID])
			if _, err := c.db.NurtureEnrollment.Create().
				SetTenantID(ld.TenantID).
				SetSequenceID(seqID).
				SetLeadID(ld.ID).
				SetCurrentStep(0).
				SetStatus("active").
				SetNextStepAt(start).
				SetMetadata(map[string]any{"source": "converted_cron"}).
				Save(ctx); err != nil {
				c.logger.Warn("converted enrol failed", zap.String("lead", ld.ID.String()), zap.Error(err))
				continue
			}
			enrolled++
			perSeq[seqID]++
		}
		if len(rows) < reengagementBatch {
			break
		}
		after = rows[len(rows)-1].ID
	}
	for seqID, n := range perSeq {
		_, _ = c.db.NurtureSequence.UpdateOneID(seqID).AddTotalEnrolled(n).Save(ctx)
	}
	return enrolled, nil
}
