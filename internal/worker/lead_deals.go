package worker

import (
	"context"
	"time"

	sharedcache "github.com/Bengo-Hub/cache"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/modules/leads"
)

// leadDealsEvery is how often missed deals are looked for.
const leadDealsEvery = time.Hour

// RunLeadDeals makes the deals of qualified and converted leads that were missed when the lead was
// saved.
func RunLeadDeals(ctx context.Context, db *ent.Client, logger *zap.Logger) {
	svc, log := leads.NewService(db, logger), logger.Named("lead-deals")
	t := time.NewTicker(leadDealsEvery)
	defer t.Stop()
	for {
		if sharedcache.ClaimPeriod(ctx, "marketflow:lead-deals", leadDealsEvery) {
			if n, err := svc.RepairDeals(ctx, time.Now().UTC()); err != nil {
				log.Error("lead deal repair failed", zap.Int("made_before_error", n), zap.Error(err))
			} else if n > 0 {
				log.Info("missed lead deals made", zap.Int("count", n))
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
