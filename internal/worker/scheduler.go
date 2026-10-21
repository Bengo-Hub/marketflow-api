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
	js     nats.JetStreamContext
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
	// JetStream (acked) publish: the NurtureStepHandler is a durable JS consumer, so
	// the signal must land in the stream — a core publish is unacked and silently
	// droppable. Core nc kept as fallback when JS init fails.
	var js nats.JetStreamContext
	if nc != nil {
		if j, err := nc.JetStream(); err == nil {
			js = j
		}
	}
	return &Scheduler{db: db, nc: nc, js: js, cfg: cfg, logger: logger.Named("scheduler")}
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
		// Oldest due first, so a backlog larger than one batch drains in order instead of
		// starving early steps; served by the (status, next_step_at) index.
		Order(ent.Asc(nurtureenrollment.FieldNextStepAt)).
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
		// Claim the enrollment BEFORE publishing: one conditional UPDATE (still active and
		// still due) that only one replica can win. Publishing first and clearing next_step_at
		// afterwards let two replicas ticking together both send the same nurture step.
		claimed, err := s.db.NurtureEnrollment.Update().
			Where(
				nurtureenrollment.IDEQ(e.ID),
				nurtureenrollment.StatusEQ("active"),
				nurtureenrollment.NextStepAtLTE(now),
			).
			SetLastStepAt(now).
			ClearNextStepAt().
			Save(ctx)
		if err != nil {
			s.logger.Warn("claim enrollment", zap.String("enrollment", e.ID.String()), zap.Error(err))
			continue
		}
		if claimed == 0 {
			continue // another replica took it
		}
		body, _ := json.Marshal(payload)
		if err := s.publish("nurture.step.due", body); err != nil {
			s.logger.Error("publish nurture.step.due", zap.String("enrollment", e.ID.String()), zap.Error(err))
			// Give it back so the next tick retries; the handler sets next_step_at on success.
			if e.NextStepAt != nil {
				_, _ = s.db.NurtureEnrollment.UpdateOneID(e.ID).SetNextStepAt(*e.NextStepAt).Save(ctx)
			}
			continue
		}
	}
}

// publish sends the work signal via JetStream (acked into the worker stream) with a
// core-NATS fallback when JS is unavailable.
func (s *Scheduler) publish(subject string, body []byte) error {
	if s.js != nil {
		_, err := s.js.Publish(subject, body)
		return err
	}
	return s.nc.Publish(subject, body)
}
