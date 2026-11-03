package publishing

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Worker claims due publish jobs and steps them. Uploads stream one chunk at a time, so two
// jobs in flight cost about 2 x ChunkSize of memory.
type Worker struct {
	Store    *EntStore
	Runner   *Runner
	Logger   *zap.Logger
	Every    time.Duration
	Batch    int
	Parallel int
	// OnFailed tells the owner when a job fails for good; nil skips it.
	OnFailed func(ctx context.Context, j *Job)
	kick     chan struct{}
}

func NewWorker(store *EntStore, runner *Runner, logger *zap.Logger) *Worker {
	return &Worker{Store: store, Runner: runner, Logger: logger.Named("publishing"),
		Every: 15 * time.Second, Batch: 10, Parallel: 2, kick: make(chan struct{}, 1)}
}

// Kick runs a tick now (a post published "now" from the API); it never blocks.
func (w *Worker) Kick() {
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

func (w *Worker) Run(ctx context.Context) {
	t := time.NewTicker(w.Every)
	defer t.Stop()
	for {
		w.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-w.kick:
		}
	}
}

func (w *Worker) tick(ctx context.Context) {
	jobs, err := w.Store.Claim(ctx, time.Now().UTC(), w.Batch)
	if err != nil {
		w.Logger.Error("claim publish jobs", zap.Error(err))
		return
	}
	sem := make(chan struct{}, max(w.Parallel, 1))
	var wg sync.WaitGroup
	for _, j := range jobs {
		sem <- struct{}{}
		wg.Add(1)
		go func(j *Job) {
			defer func() { <-sem; wg.Done() }()
			if err := w.Runner.Step(ctx, j); err != nil && !errors.Is(err, ErrCancelled) {
				w.Logger.Error("publish step", zap.String("job", j.ID), zap.String("platform", j.Platform), zap.Error(err))
			}
			if j.Status == StatusFailed && w.OnFailed != nil {
				w.OnFailed(ctx, j)
			}
			if j.Status == StatusPublished || j.Status == StatusFailed {
				w.Logger.Info("publish job done", zap.String("job", j.ID), zap.String("platform", j.Platform),
					zap.String("status", j.Status), zap.String("error", j.Error))
			}
		}(j)
	}
	wg.Wait()
}
