package worker

import (
	"context"
	"encoding/json"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/nurtureenrollment"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

// Scheduler periodically publishes `nurture.step.due` events for enrollments
// whose next_step_at has arrived. The NurtureStepHandler consumer takes it
// from there (dispatch via notifications-api + advance the enrollment).
type Scheduler struct {
	db     *ent.Client
	nc     *nats.Conn
	cfg    SchedulerConfig
	logger *zap.Logger
}

// NewScheduler constructs a Scheduler.
func NewScheduler(db *ent.Client, nc *nats.Conn, cfg SchedulerConfig, logger *zap.Logger) *Scheduler {
	if cfg.IntervalSeconds <= 0 {
		cfg.IntervalSeconds = 60
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 50
	}
	return &Scheduler{db: db, nc: nc, cfg: cfg, logger: logger.Named("scheduler")}
}

// Run blocks until ctx is cancelled, ticking every IntervalSeconds.
func (s *Scheduler) Run(ctx context.Context) {
	tick := time.NewTicker(time.Duration(s.cfg.IntervalSeconds) * time.Second)
	defer tick.Stop()
	s.logger.Info("nurture scheduler started",
		zap.Int("interval_s", s.cfg.IntervalSeconds),
		zap.Int("batch", s.cfg.BatchSize),
	)
	// Fire once immediately so newly-created enrollments don't wait a full tick.
	s.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			s.logger.Info("nurture scheduler stopped")
			return
		case <-tick.C:
			s.tick(ctx)
		}
	}
}

func (s *Scheduler) tick(ctx context.Context) {
	now := time.Now().UTC()
	rows, err := s.db.NurtureEnrollment.Query().
		Where(
			nurtureenrollment.StatusEQ("active"),
			nurtureenrollment.NextStepAtLTE(now),
		).
		Limit(s.cfg.BatchSize).
		All(ctx)
	if err != nil {
		s.logger.Error("scheduler query failed", zap.Error(err))
		return
	}
	if len(rows) == 0 {
		return
	}
	s.logger.Debug("enrollments due", zap.Int("count", len(rows)))

	for _, e := range rows {
		payload := map[string]any{
			"tenant_id":     e.TenantID.String(),
			"sequence_id":   e.SequenceID.String(),
			"enrollment_id": e.ID.String(),
			"step_index":    e.CurrentStep,
		}
		if e.LeadID != nil {
			payload["lead_id"] = e.LeadID.String()
		}
		// The NurtureStepHandler reads recipient/channel/template/variables from
		// the sequence definition + lead row after decoding; we include enrollment
		// minimal context here and let the handler enrich.
		body, _ := json.Marshal(payload)
		if err := s.nc.Publish("nurture.step.due", body); err != nil {
			s.logger.Error("publish nurture.step.due", zap.String("enrollment", e.ID.String()), zap.Error(err))
			continue
		}
		// Optimistically bump last_step_at so a slow handler doesn't cause
		// re-fires. The handler is responsible for setting next_step_at after
		// successful dispatch.
		if _, err := s.db.NurtureEnrollment.UpdateOneID(e.ID).
			SetLastStepAt(now).
			ClearNextStepAt().
			Save(ctx); err != nil {
			s.logger.Warn("update enrollment pre-dispatch", zap.Error(err))
		}
	}
}

