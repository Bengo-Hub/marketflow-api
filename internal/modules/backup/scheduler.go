package backup

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// schedulerAdvisoryLockKey is a fixed, service-unique key for the Postgres session advisory
// lock that guards the daily run so only ONE replica executes it. ("marketflow-backup")
const schedulerAdvisoryLockKey int64 = 0x4D4B_4642 // 'M','K','F','B'

// SchedulerConfig configures the daily auto-backup + retention churn.
type SchedulerConfig struct {
	Enabled       bool // BACKUP_SCHEDULE_ENABLED (default true)
	Hour          int  // BACKUP_SCHEDULE_HOUR (default 2) — service-local time
	RetentionDays int  // BACKUP_RETENTION_DAYS (default 4)
}

// Scheduler runs the per-tenant opt-in auto-backup + a retention churn, using a
// time-until-next-run timer loop (no external cron dep) and a Postgres advisory lock so
// only one replica performs the work.
type Scheduler struct {
	svc *Service
	cfg SchedulerConfig
	log *zap.Logger
}

// NewScheduler builds the scheduler. Defaults are applied for zero-value config.
func NewScheduler(svc *Service, cfg SchedulerConfig, log *zap.Logger) *Scheduler {
	if cfg.Hour < 0 || cfg.Hour > 23 {
		cfg.Hour = 2
	}
	if cfg.RetentionDays <= 0 {
		cfg.RetentionDays = DefaultRetentionDays
	}
	return &Scheduler{svc: svc, cfg: cfg, log: log.Named("backup.Scheduler")}
}

// Start launches the scheduler goroutine: a churn on startup, then wakes hourly and backs
// up only the tenants that opted in for the current hour. Stops when ctx is cancelled.
func (sc *Scheduler) Start(ctx context.Context) {
	if !sc.cfg.Enabled {
		sc.log.Info("backup scheduler disabled (BACKUP_SCHEDULE_ENABLED=false)")
		return
	}
	sc.log.Info("backup scheduler started",
		zap.Int("hour", sc.cfg.Hour),
		zap.Int("retention_days", sc.cfg.RetentionDays))

	go func() {
		// Startup churn (replica-guarded); backupHour=-1 skips the backup pass.
		sc.runGuarded(ctx, -1)
		for {
			next := nextTopOfHour(time.Now())
			timer := time.NewTimer(time.Until(next))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
				// Wake hourly; back up only tenants whose ACTIVATED setting matches this hour.
				sc.runGuarded(ctx, time.Now().Hour())
			}
		}
	}()
}

// runGuarded acquires the advisory lock and, if won, backs up the tenants ACTIVATED for
// backupHour (skipped when backupHour < 0) followed by the safety churn. Only one replica
// wins the lock per tick.
func (sc *Scheduler) runGuarded(ctx context.Context, backupHour int) {
	conn, err := sc.svc.db.Conn(ctx)
	if err != nil {
		sc.log.Warn("scheduler: acquire conn failed", zap.Error(err))
		return
	}
	defer func() { _ = conn.Close() }()

	var got bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, schedulerAdvisoryLockKey).Scan(&got); err != nil {
		sc.log.Warn("scheduler: advisory lock failed", zap.Error(err))
		return
	}
	if !got {
		return
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, schedulerAdvisoryLockKey)
	}()

	if backupHour >= 0 {
		sc.backupActivatedTenants(ctx, backupHour)
	}
	// Safety churn across all tenants at the global default retention (per-tenant churn
	// also runs right after each tenant's own backup, using that tenant's retention).
	if _, err := sc.svc.Churn(ctx, sc.cfg.RetentionDays); err != nil {
		sc.log.Warn("scheduler: churn failed", zap.Error(err))
	}
}

// backupActivatedTenants backs up ONLY tenants that have OPTED IN to auto-backup for this
// hour (BackupSetting.auto_enabled=true && schedule_hour==hour). Tenants without a settings
// row (the default) are never auto-backed-up. Each is churned to its own retention.
func (sc *Scheduler) backupActivatedTenants(ctx context.Context, hour int) {
	tenants, err := sc.svc.ListActivatedTenants(ctx, hour)
	if err != nil {
		sc.log.Warn("scheduler: list activated tenants failed", zap.Error(err))
		return
	}
	ok := 0
	for _, t := range tenants {
		if _, err := sc.svc.Generate(ctx, t.TenantID); err != nil {
			sc.log.Warn("scheduler: tenant backup failed", zap.String("tenant", t.TenantID.String()), zap.Error(err))
			continue
		}
		_, _ = sc.svc.ChurnTenant(ctx, t.TenantID, t.RetentionDays)
		ok++
	}
	if len(tenants) > 0 {
		sc.log.Info("scheduled backup complete", zap.Int("hour", hour), zap.Int("activated", len(tenants)), zap.Int("succeeded", ok))
	}
}

// nextTopOfHour returns the next HH:00 strictly after now (service-local time).
func nextTopOfHour(now time.Time) time.Time {
	return now.Truncate(time.Hour).Add(time.Hour)
}
