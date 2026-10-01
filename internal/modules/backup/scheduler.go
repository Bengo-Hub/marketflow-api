package backup

import (
	"context"
	"time"

	"go.uber.org/zap"

	sharedcache "github.com/Bengo-Hub/cache"
	"github.com/redis/go-redis/v9"
)

// schedulerLockPrefix keys the Redis lease that runs each scheduler tick on one replica.
const schedulerLockPrefix = "marketflow:backup:tick"

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
	rdb redis.UniversalClient
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

// runGuarded runs one tick on a single replica. Hourly ticks run once per hour fleet-wide
// (RunOnce keyed by the hour, so a replica whose timer fires a moment later cannot repeat
// it); the startup churn (backupHour -1) only needs mutual exclusion. This replaced a
// session pg_try_advisory_lock, which PgBouncer transaction pooling breaks (lock and unlock
// can land on different server connections, leaking the lock or admitting a second replica).
func (sc *Scheduler) runGuarded(ctx context.Context, backupHour int) {
	tick := func(ctx context.Context) error { return sc.runTick(ctx, backupHour) }
	var ran bool
	var err error
	if backupHour < 0 {
		ran, err = sharedcache.RunExclusive(ctx, sc.rdb, sc.log, schedulerLockPrefix+":startup", 30*time.Minute, tick)
	} else {
		ran, err = sharedcache.RunOnce(ctx, sc.rdb, sc.log, sharedcache.PeriodKey(schedulerLockPrefix, time.Hour), time.Hour, tick)
	}
	if err != nil {
		sc.log.Warn("scheduler: tick not run", zap.Bool("ran", ran), zap.Error(err))
	}
}

// runTick backs up the tenants that activated auto-backup for backupHour (when >= 0), then
// runs the retention churn.
func (sc *Scheduler) runTick(ctx context.Context, backupHour int) error {
	if backupHour >= 0 {
		sc.backupActivatedTenants(ctx, backupHour)
	}
	// Safety churn across all tenants at the global default retention (per-tenant churn
	// also runs right after each tenant's own backup, using that tenant's retention).
	if _, err := sc.svc.Churn(ctx, sc.cfg.RetentionDays); err != nil {
		sc.log.Warn("scheduler: churn failed", zap.Error(err))
	}
	return nil
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

// WithRedis sets the Redis client used for the cross-replica tick lease. Without it no tick
// runs (logged), because running on every replica would duplicate backups.
func (sc *Scheduler) WithRedis(rdb redis.UniversalClient) *Scheduler {
	sc.rdb = rdb
	return sc
}
