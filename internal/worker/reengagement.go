package worker

import (
	"context"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/ent/nurtureenrollment"
	"github.com/bengobox/marketflow-api/internal/ent/nurturesequence"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// ReEngagement is a daily cron that finds dormant leads (no activity in the
// configured window) and enrols them into the tenant's active sequence whose
// trigger_event == "contact.dormant". Idempotent: skips leads that already
// have an active enrollment on the same sequence.
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
	cutoff := time.Now().UTC().Add(-r.dormant)

	// Eligible leads: created before cutoff, no activity since, not converted/lost.
	rows, err := r.db.Lead.Query().
		Where(
			lead.CreatedAtLT(cutoff),
			lead.StatusNotIn("converted", "lost"),
		).
		Limit(500).
		All(ctx)
	if err != nil {
		r.logger.Error("dormant query failed", zap.Error(err))
		return
	}
	if len(rows) == 0 {
		return
	}
	enrolled := 0
	for _, ld := range rows {
		// Pick the tenant's active re-engagement sequence.
		seq, err := r.db.NurtureSequence.Query().
			Where(
				nurturesequence.TenantIDEQ(ld.TenantID),
				nurturesequence.TriggerEventEQ("contact.dormant"),
				nurturesequence.ActiveEQ(true),
			).
			First(ctx)
		if err != nil {
			continue // no re-engagement sequence configured for this tenant
		}
		// Already enrolled on this sequence? Skip.
		exists, err := r.db.NurtureEnrollment.Query().
			Where(
				nurtureenrollment.TenantIDEQ(ld.TenantID),
				nurtureenrollment.SequenceIDEQ(seq.ID),
				nurtureenrollment.LeadIDEQ(ld.ID),
				nurtureenrollment.StatusIn("active", "paused"),
			).
			Exist(ctx)
		if err != nil || exists {
			continue
		}
		_, err = r.db.NurtureEnrollment.Create().
			SetTenantID(ld.TenantID).
			SetSequenceID(seq.ID).
			SetLeadID(ld.ID).
			SetCurrentStep(0).
			SetStatus("active").
			SetNextStepAt(time.Now().UTC()).
			SetMetadata(map[string]any{"source": "reengagement_cron"}).
			Save(ctx)
		if err == nil {
			enrolled++
			_, _ = r.db.NurtureSequence.UpdateOneID(seq.ID).AddTotalEnrolled(1).Save(ctx)
		}
	}
	if enrolled > 0 {
		r.logger.Info("re-engagement enrollments created", zap.Int("count", enrolled))
	}
}

// Ensure uuid import is not flagged as unused when the file is ever reduced.
var _ = uuid.Nil
