package worker

import (
	"context"
	"time"

	sharedcache "github.com/Bengo-Hub/cache"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/chatsession"
	"github.com/bengobox/marketflow-api/internal/ent/predicate"
)

// Chat transcripts are kept for 30 days after their last activity (Ads Autopilot plan, section
// 4.5; ERD retention table). marketflow-ai stamps last_activity_at on every message. Rows from
// before that column existed have no stamp, so they are pruned only when closed and started
// before the cutoff, or when they started before a longer legacy cutoff. A legacy session that is
// resumed gets stamped by its next message and from then on follows the normal rule.
const (
	chatRetentionBatch      = 500
	chatRetentionMaxBatches = 40 // at most 20,000 rows per run, so one run never holds the DB long
	legacyChatGraceFactor   = 3  // legacy rows with no stamp: 3 x retention (90 days by default)
)

// ChatRetention deletes chat sessions past the retention window, once a day fleet-wide.
type ChatRetention struct {
	db        *ent.Client
	retention time.Duration
	interval  time.Duration
	logger    *zap.Logger
}

// NewChatRetention builds the prune job. retentionDays <= 0 disables it.
func NewChatRetention(db *ent.Client, retentionDays int, logger *zap.Logger) *ChatRetention {
	return &ChatRetention{
		db:        db,
		retention: time.Duration(retentionDays) * 24 * time.Hour,
		interval:  24 * time.Hour,
		logger:    logger.Named("chat-retention"),
	}
}

// Run blocks until ctx is cancelled, pruning once at start and then daily.
func (c *ChatRetention) Run(ctx context.Context) {
	if c.retention <= 0 {
		c.logger.Info("chat retention disabled")
		return
	}
	t := time.NewTicker(c.interval)
	defer t.Stop()
	c.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.tick(ctx)
		}
	}
}

func (c *ChatRetention) tick(ctx context.Context) {
	if !sharedcache.ClaimPeriod(ctx, "marketflow:chat-retention", c.interval) {
		return
	}
	deleted, err := c.prune(ctx, time.Now().UTC())
	if err != nil {
		c.logger.Error("chat retention prune failed", zap.Int("deleted_before_error", deleted), zap.Error(err))
		return
	}
	if deleted > 0 {
		c.logger.Info("chat sessions pruned", zap.Int("deleted", deleted))
	}
}

// expiredSessions is the retention rule as one predicate (exported to tests via prune).
func expiredSessions(now time.Time, retention time.Duration) predicate.ChatSession {
	cutoff := now.Add(-retention)
	legacyCutoff := now.Add(-retention * legacyChatGraceFactor)
	return chatsession.Or(
		chatsession.LastActivityAtLT(cutoff),
		chatsession.And(
			chatsession.LastActivityAtIsNil(),
			chatsession.StartedAtLT(cutoff),
			chatsession.Or(chatsession.StatusEQ("closed"), chatsession.EndedAtNotNil()),
		),
		chatsession.And(
			chatsession.LastActivityAtIsNil(),
			chatsession.StartedAtLT(legacyCutoff),
		),
	)
}

// prune deletes expired sessions in bounded batches and returns how many it removed.
func (c *ChatRetention) prune(ctx context.Context, now time.Time) (int, error) {
	total := 0
	for i := 0; i < chatRetentionMaxBatches; i++ {
		ids, err := c.db.ChatSession.Query().
			Where(expiredSessions(now, c.retention)).
			Limit(chatRetentionBatch).
			IDs(ctx)
		if err != nil {
			return total, err
		}
		if len(ids) == 0 {
			return total, nil
		}
		n, err := c.db.ChatSession.Delete().Where(chatsession.IDIn(ids...)).Exec(ctx)
		if err != nil {
			return total, err
		}
		total += n
		if len(ids) < chatRetentionBatch {
			return total, nil
		}
	}
	return total, nil
}
