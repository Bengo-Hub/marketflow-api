package creative

import (
	"context"
	"os"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/creativeasset"
	"github.com/bengobox/marketflow-api/internal/ent/creativebrief"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
)

// Discarded drafts (owner, 2026-11-14): a WhatsApp Discard keeps the Studio draft for a while,
// in case the tap was a mistake, then hard deletes it (row and files) when nobody acted on it,
// so storage stays clean. Acting on it (an edit, a storyboard, a render, an approval) after the
// discard keeps it.

// OSTurnDiscarded is marketflow-ai reporting a WhatsApp Discard tap for a turn's video.
const OSTurnDiscarded = "discarded"

// metaDiscardedAt records when the owner discarded a draft.
const metaDiscardedAt = "discarded_at"

// DiscardPurgeAfter is how long a discarded draft is kept before it is deleted:
// MF_CREATIVE_DISCARD_PURGE_AFTER (a Go duration), default DraftTTL.
var DiscardPurgeAfter = discardWindowFromEnv(os.Getenv("MF_CREATIVE_DISCARD_PURGE_AFTER"))

func discardWindowFromEnv(v string) time.Duration {
	if d, err := time.ParseDuration(v); err == nil && d >= time.Hour {
		return d
	}
	return DraftTTL
}

// MarkDiscarded records the discard of a draft that was never approved, and makes its files
// expire at the end of the window (sooner only, never later). An approved or unknown version
// is left as it is; ok reports whether the mark was made.
func (s *Service) MarkDiscarded(ctx context.Context, tenantID, id uuid.UUID, window time.Duration) (bool, error) {
	v, err := s.db.CreativeVersion.Query().
		Where(creativeversion.ID(id), creativeversion.TenantID(tenantID), creativeversion.ApprovedAtIsNil()).
		Only(ctx)
	if ent.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	now := s.nowFn().UTC()
	meta := copyMap(v.Metadata)
	meta[metaDiscardedAt] = now.Format(time.RFC3339)
	n, err := s.db.CreativeVersion.Update().
		Where(creativeversion.ID(id), creativeversion.TenantID(tenantID), creativeversion.ApprovedAtIsNil()).
		SetMetadata(meta).Save(ctx)
	if err != nil || n == 0 {
		return false, err
	}
	until := now.Add(window)
	_, err = s.db.CreativeAsset.Update().
		Where(creativeasset.VersionID(id), creativeasset.PurgedAtIsNil(),
			creativeasset.Or(creativeasset.ExpiresAtIsNil(), creativeasset.ExpiresAtGT(until))).
		SetExpiresAt(until).Save(ctx)
	return true, err
}

// discardedAt reads the discard time of a version ("" or unreadable: zero).
func discardedAt(meta map[string]any) time.Time {
	t, _ := time.Parse(time.RFC3339, str(meta[metaDiscardedAt]))
	return t
}

// actedOnSince is how much later than the discard a version may be updated and still count as
// untouched (the discard's own write and a job callback settling at the same moment).
const actedOnSince = time.Minute

// PurgeDiscarded hard deletes discarded drafts whose window is over, that nobody acted on since
// and whose files are all gone: the asset rows, the version, and the brief when it has no other
// version left. It returns how many versions went.
func (p *Purger) PurgeDiscarded(ctx context.Context, window time.Duration) (int, error) {
	cutoff := p.nowFn().UTC().Add(-window)
	rows, err := p.db.CreativeVersion.Query().
		Where(creativeversion.ApprovedAtIsNil(), func(s *entsql.Selector) {
			col := s.C(creativeversion.FieldMetadata)
			// The builder numbers the argument for Postgres ($n); a raw "?" was sent as is.
			s.Where(entsql.P(func(b *entsql.Builder) {
				b.WriteString("(" + col + "->>'" + metaDiscardedAt + "') IS NOT NULL AND (" + col + "->>'" + metaDiscardedAt + "')::timestamptz < ")
				b.Arg(cutoff)
			}))
		}).
		Limit(purgeBatch).All(ctx)
	if err != nil {
		return 0, err
	}
	done := 0
	for _, v := range rows {
		at := discardedAt(v.Metadata)
		if at.IsZero() || v.UpdatedAt.After(at.Add(actedOnSince)) {
			continue // acted on after the discard: kept
		}
		live, err := p.db.CreativeAsset.Query().
			Where(creativeasset.VersionID(v.ID), creativeasset.PurgedAtIsNil()).Exist(ctx)
		if err != nil {
			return done, err
		}
		if live {
			continue // the file purge removes them first; next pass
		}
		if err := p.deleteVersion(ctx, v); err != nil {
			p.logger.Warn("discarded draft not deleted", zap.String("version_id", v.ID.String()), zap.Error(err))
			continue
		}
		done++
	}
	return done, nil
}

func (p *Purger) deleteVersion(ctx context.Context, v *ent.CreativeVersion) error {
	tx, err := p.db.Tx(ctx)
	if err != nil {
		return err
	}
	if _, err := tx.CreativeAsset.Delete().Where(creativeasset.VersionID(v.ID)).Exec(ctx); err != nil {
		_ = tx.Rollback()
		return err
	}
	n, err := tx.CreativeVersion.Delete().
		Where(creativeversion.ID(v.ID), creativeversion.ApprovedAtIsNil(), creativeversion.UpdatedAtLTE(v.UpdatedAt)).
		Exec(ctx)
	if err != nil || n == 0 {
		_ = tx.Rollback()
		return err // n == 0: changed meanwhile, kept
	}
	others, err := tx.CreativeVersion.Query().Where(creativeversion.BriefID(v.BriefID)).Exist(ctx)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if !others {
		if _, err := tx.CreativeBrief.Delete().Where(creativebrief.ID(v.BriefID)).Exec(ctx); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}
