package funnels

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/campaign"
	"github.com/bengobox/marketflow-api/internal/ent/funnel"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/platform/svcconfig"
)

// Funnel retention (owner 2026-11-20): a funnel that has served its purpose is archived, then
// deleted for good either by an admin or automatically once it has been archived longer than the
// configured window. Its leads stay: they keep the funnel's name in their metadata and lose only
// the link (leads.funnel_id is ON DELETE SET NULL); campaigns pointing at it are unlinked.

// ArchivePurgeKey is the service-config key for how long an archived funnel is kept before the
// automatic hard delete: a Go duration ("720h") or hours. Tenant row over platform row.
const ArchivePurgeKey = "funnels.archive_purge_after"

// Fallback and bounds of the window; change the setting, not these.
const (
	DefaultArchivePurgeAfter = 30 * 24 * time.Hour
	minArchivePurgeAfter     = time.Hour
	maxArchivePurgeAfter     = 365 * 24 * time.Hour
)

const (
	metaArchivedAt = "archived_at"
	// metaLeadFunnelName keeps the deleted funnel's name on its leads for attribution history.
	metaLeadFunnelName = "funnel_name"
	purgeBatch         = 200
)

// ErrNotArchived is a permanent delete of a funnel that is not archived.
var ErrNotArchived = errors.New("only an archived funnel can be deleted permanently; archive it first")

// ArchiveWindow is the tenant's window before an archived funnel is deleted for good.
func (s *Service) ArchiveWindow(ctx context.Context, tenantID uuid.UUID) time.Duration {
	return svcconfig.Duration(ctx, s.client, tenantID, ArchivePurgeKey, DefaultArchivePurgeAfter, minArchivePurgeAfter, maxArchivePurgeAfter)
}

// markArchived records when a funnel was archived (metadata.archived_at), so the window counts
// from the archive and not from the last edit.
func (s *Service) markArchived(ctx context.Context, tenantID, funnelID uuid.UUID, at time.Time) error {
	patch, _ := json.Marshal(map[string]string{metaArchivedAt: at.UTC().Format(time.RFC3339)})
	return s.client.Funnel.Update().
		Where(funnel.ID(funnelID), funnel.TenantID(tenantID)).
		Modify(func(u *entsql.UpdateBuilder) {
			u.Set(funnel.FieldMetadata, entsql.ExprFunc(func(b *entsql.Builder) {
				b.WriteString("(COALESCE(" + funnel.FieldMetadata + ", '{}'::jsonb) || ")
				b.Arg(string(patch))
				b.WriteString("::jsonb)")
			}))
		}).Exec(ctx)
}

// Purge deletes an archived funnel for good (admins; the route is admin only).
func (s *Service) Purge(ctx context.Context, tenantID, funnelID uuid.UUID) error {
	f, err := s.client.Funnel.Query().Where(funnel.ID(funnelID), funnel.TenantID(tenantID)).Only(ctx)
	if err != nil {
		return err
	}
	if f.Status != "archived" {
		return ErrNotArchived
	}
	return s.hardDelete(ctx, f)
}

// hardDelete unlinks the funnel's leads (keeping its name on them) and campaigns, then deletes it.
func (s *Service) hardDelete(ctx context.Context, f *ent.Funnel) error {
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	name, _ := json.Marshal(map[string]string{metaLeadFunnelName: f.Name})
	if err := tx.Lead.Update().
		Where(lead.FunnelID(f.ID), lead.TenantID(f.TenantID)).
		Modify(func(u *entsql.UpdateBuilder) {
			u.Set(lead.FieldMetadata, entsql.ExprFunc(func(b *entsql.Builder) {
				b.WriteString("(COALESCE(" + lead.FieldMetadata + ", '{}'::jsonb) || ")
				b.Arg(string(name))
				b.WriteString("::jsonb)")
			}))
		}).ClearFunnelID().Exec(ctx); err != nil {
		return fmt.Errorf("unlink leads: %w", err)
	}
	if err := tx.Campaign.Update().Where(campaign.FunnelID(f.ID), campaign.TenantID(f.TenantID)).ClearFunnelID().Exec(ctx); err != nil {
		return fmt.Errorf("unlink campaigns: %w", err)
	}
	if err := tx.Funnel.DeleteOneID(f.ID).Exec(ctx); err != nil {
		return fmt.Errorf("delete funnel: %w", err)
	}
	return tx.Commit()
}

// PurgeArchived deletes archived funnels whose tenant window is over; it returns how many went.
// A funnel archived before archived_at was recorded counts from its last update.
func (s *Service) PurgeArchived(ctx context.Context, now time.Time) (int, error) {
	return s.purgeArchived(ctx, now, uuid.Nil)
}

// purgeArchived is PurgeArchived limited to one tenant when tenantID is set (tests use it so they
// never touch other tenants' rows).
func (s *Service) purgeArchived(ctx context.Context, now time.Time, tenantID uuid.UUID) (int, error) {
	q := s.client.Funnel.Query().Where(funnel.Status("archived"))
	if tenantID != uuid.Nil {
		q = q.Where(funnel.TenantID(tenantID))
	}
	rows, err := q.Order(ent.Asc(funnel.FieldUpdatedAt)).Limit(purgeBatch).All(ctx)
	if err != nil {
		return 0, err
	}
	windows := map[uuid.UUID]time.Duration{}
	done := 0
	for _, f := range rows {
		w, ok := windows[f.TenantID]
		if !ok {
			w = s.ArchiveWindow(ctx, f.TenantID)
			windows[f.TenantID] = w
		}
		since := f.UpdatedAt
		if v, ok := f.Metadata[metaArchivedAt].(string); ok {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				since = t
			}
		}
		if now.Sub(since) < w {
			continue
		}
		if err := s.hardDelete(ctx, f); err != nil {
			s.logger.Warn("archived funnel not deleted", zap.String("funnel", f.ID.String()), zap.Error(err))
			continue
		}
		done++
	}
	return done, nil
}

// RunPurge deletes due archived funnels every interval until ctx ends (the worker runs it).
func (s *Service) RunPurge(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Hour
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := s.PurgeArchived(ctx, time.Now()); err != nil {
				s.logger.Warn("archived funnel purge failed", zap.Error(err))
			} else if n > 0 {
				s.logger.Info("archived funnels deleted", zap.Int("funnels", n))
			}
		}
	}
}
