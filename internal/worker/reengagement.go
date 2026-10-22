package worker

import (
	"context"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	sharedcache "github.com/Bengo-Hub/cache"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/ent/nurtureenrollment"
	"github.com/bengobox/marketflow-api/internal/ent/nurturesequence"
	"github.com/bengobox/marketflow-api/internal/ent/predicate"
)

const (
	reengagementBatch      = 500
	reengagementMaxBatches = 20 // at most 10,000 enrolments per run
)

// ReEngagement is a daily cron that finds dormant leads (created and not updated within the
// configured window) and enrols them into the tenant's active sequence whose trigger_event is
// "contact.dormant". A lead is enrolled in that sequence at most once, ever: one that already
// went through it (any status, including completed) is never re-enrolled.
type ReEngagement struct {
	db       *ent.Client
	interval time.Duration
	dormant  time.Duration
	logger   *zap.Logger
}

// NewReEngagement constructs a ReEngagement runner.
// interval: how often to scan (default 24h). dormant: how long since last
// activity before a lead qualifies (default 60 days).
func NewReEngagement(db *ent.Client, interval, dormant time.Duration, logger *zap.Logger) *ReEngagement {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	if dormant <= 0 {
		dormant = 60 * 24 * time.Hour
	}
	return &ReEngagement{
		db:       db,
		interval: interval,
		dormant:  dormant,
		logger:   logger.Named("reengagement"),
	}
}

// Run blocks until ctx is cancelled, firing tick immediately and then every
// `interval`.
func (r *ReEngagement) Run(ctx context.Context) {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	r.logger.Info("re-engagement cron started",
		zap.Duration("interval", r.interval),
		zap.Duration("dormant_window", r.dormant),
	)
	r.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.tick(ctx)
		}
	}
}

func (r *ReEngagement) tick(ctx context.Context) {
	// Runs on every replica's ticker; only the first replica in each period does the work.
	if !sharedcache.ClaimPeriod(ctx, "marketflow:reengagement", r.interval) {
		return
	}
	enrolled, err := r.enrolDormant(ctx, time.Now().UTC())
	if err != nil {
		r.logger.Error("re-engagement run failed", zap.Int("enrolled_before_error", enrolled), zap.Error(err))
		return
	}
	if enrolled > 0 {
		r.logger.Info("re-engagement enrollments created", zap.Int("count", enrolled))
	}
}

// enrolDormant does one run: it walks every eligible lead once, in id order, with all filters
// in SQL, and returns how many enrolments it created.
func (r *ReEngagement) enrolDormant(ctx context.Context, now time.Time) (int, error) {
	// One sequence per tenant: the oldest active contact.dormant sequence.
	seqs, err := r.db.NurtureSequence.Query().
		Where(
			nurturesequence.TriggerEventEQ("contact.dormant"),
			nurturesequence.ActiveEQ(true),
		).
		Order(ent.Asc(nurturesequence.FieldCreatedAt), ent.Asc(nurturesequence.FieldID)).
		All(ctx)
	if err != nil {
		return 0, err
	}
	seqByTenant := make(map[uuid.UUID]uuid.UUID, len(seqs))
	for _, s := range seqs {
		if _, ok := seqByTenant[s.TenantID]; !ok {
			seqByTenant[s.TenantID] = s.ID
		}
	}
	return r.enrolForSequences(ctx, now, seqByTenant)
}

// enrolForSequences enrols the dormant leads of the given tenants into their sequence.
func (r *ReEngagement) enrolForSequences(ctx context.Context, now time.Time, seqByTenant map[uuid.UUID]uuid.UUID) (int, error) {
	if len(seqByTenant) == 0 {
		return 0, nil
	}
	tenantIDs := make([]uuid.UUID, 0, len(seqByTenant))
	seqIDs := make([]uuid.UUID, 0, len(seqByTenant))
	for t, s := range seqByTenant {
		tenantIDs = append(tenantIDs, t)
		seqIDs = append(seqIDs, s)
	}

	cutoff := now.Add(-r.dormant)
	enrolled := 0
	perSeq := make(map[uuid.UUID]int)
	after := uuid.Nil
	for i := 0; i < reengagementMaxBatches; i++ {
		rows, err := r.db.Lead.Query().
			Where(
				lead.TenantIDIn(tenantIDs...),
				lead.CreatedAtLT(cutoff),
				lead.UpdatedAtLT(cutoff),
				lead.StatusNotIn("converted", "lost"),
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
			if _, err := r.db.NurtureEnrollment.Create().
				SetTenantID(ld.TenantID).
				SetSequenceID(seqID).
				SetLeadID(ld.ID).
				SetCurrentStep(0).
				SetStatus("active").
				SetNextStepAt(now).
				SetMetadata(map[string]any{"source": "reengagement_cron"}).
				Save(ctx); err != nil {
				r.logger.Warn("re-engagement enrol failed", zap.String("lead", ld.ID.String()), zap.Error(err))
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
		_, _ = r.db.NurtureSequence.UpdateOneID(seqID).AddTotalEnrolled(n).Save(ctx)
	}
	return enrolled, nil
}

// notEnrolledIn keeps leads that have never had an enrolment (any status) in any of the given
// sequences: NOT EXISTS against nurture_enrollments, served by its (tenant_id, lead_id,
// sequence_id) index.
func notEnrolledIn(seqIDs []uuid.UUID) predicate.Lead {
	return predicate.Lead(func(s *entsql.Selector) {
		e := entsql.Table(nurtureenrollment.Table)
		ids := make([]any, len(seqIDs))
		for i, id := range seqIDs {
			ids[i] = id
		}
		s.Where(entsql.NotExists(
			entsql.Select(e.C(nurtureenrollment.FieldID)).From(e).Where(entsql.And(
				entsql.ColumnsEQ(e.C(nurtureenrollment.FieldLeadID), s.C(lead.FieldID)),
				entsql.In(e.C(nurtureenrollment.FieldSequenceID), ids...),
			)),
		))
	})
}
