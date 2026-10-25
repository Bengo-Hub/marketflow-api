package creative

import (
	"context"
	"time"

	"github.com/Bengo-Hub/httpware/objectstore"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/creativeasset"
	"go.uber.org/zap"
)

// TypeCreativeDrafts mirrors the media type marketflow-ai registers for the files
// it stores (internal/media/store.go there); the store needs it to resolve and
// delete their keys from this service.
const TypeCreativeDrafts = "creative-drafts"

// TypeAudioDrafts mirrors marketflow-ai's voice-over type (video ads).
const TypeAudioDrafts = "audio-drafts"

func init() {
	objectstore.Register(objectstore.MediaType{
		Name:         TypeCreativeDrafts,
		Public:       true,
		MaxBytes:     15 << 20,
		Allowed:      []string{"image/jpeg", "image/png"},
		CacheControl: "public, max-age=300",
	})
	objectstore.Register(objectstore.MediaType{
		Name:         TypeAudioDrafts,
		Public:       true,
		MaxBytes:     32 << 20,
		Allowed:      []string{"audio/mpeg"},
		CacheControl: "public, max-age=300",
	})
}

// purgeBatch bounds one pass; the partial index keeps each read small.
const purgeBatch = 200

// Purger deletes the files of drafts whose time is up and marks them purged.
type Purger struct {
	db     *ent.Client
	store  *objectstore.Store
	logger *zap.Logger
	nowFn  func() time.Time
}

// NewPurger builds a Purger. A nil or unconfigured store turns it off.
func NewPurger(db *ent.Client, store *objectstore.Store, logger *zap.Logger) *Purger {
	return &Purger{db: db, store: store, logger: logger.Named("creative.purge"), nowFn: time.Now}
}

// Run purges hourly until ctx ends.
func (p *Purger) Run(ctx context.Context) {
	if !p.store.Enabled() {
		p.logger.Warn("creative draft purge off: object storage is not configured")
		return
	}
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := p.Purge(ctx); err != nil {
				p.logger.Warn("creative draft purge failed", zap.Error(err))
			} else if n > 0 {
				p.logger.Info("creative drafts purged", zap.Int("files", n))
			}
		}
	}
}

// Purge deletes expired files, a batch at a time, and reports how many went.
// Replicas may overlap: deleting a missing file is not an error and the
// purged_at update is conditional.
func (p *Purger) Purge(ctx context.Context) (int, error) {
	total := 0
	for {
		rows, err := p.db.CreativeAsset.Query().
			Where(creativeasset.PurgedAtIsNil(), creativeasset.ExpiresAtLT(p.nowFn())).
			Order(ent.Asc(creativeasset.FieldExpiresAt)).
			Limit(purgeBatch).
			All(ctx)
		if err != nil || len(rows) == 0 {
			return total, err
		}
		done := 0
		for _, a := range rows {
			if err := p.store.Delete(ctx, a.StorageKey); err != nil {
				p.logger.Warn("draft file not deleted", zap.String("asset_id", a.ID.String()), zap.Error(err))
				continue
			}
			n, err := p.db.CreativeAsset.Update().
				Where(creativeasset.ID(a.ID), creativeasset.PurgedAtIsNil()).
				SetPurgedAt(p.nowFn()).
				Save(ctx)
			if err != nil {
				return total, err
			}
			done += n
		}
		total += done
		if done == 0 || len(rows) < purgeBatch {
			return total, nil
		}
	}
}
