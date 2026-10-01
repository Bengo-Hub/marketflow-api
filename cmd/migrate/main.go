// Command migrate applies MarketFlow database migrations and exits.
//
// The container entrypoint runs this binary (as a DB-readiness gate + schema apply) before starting
// the server. It mirrors the in-process migration cmd/server performs when RunMigrations is enabled,
// reusing database.RunMigrations so there is a single source of migration logic.
package main

import (
	"context"
	"log"

	"github.com/bengobox/marketflow-api/internal/config"
	"github.com/bengobox/marketflow-api/internal/database"
	"github.com/joho/godotenv"
)

// migrationLockKey serializes migrations across pods: every replica's entrypoint runs this
// binary and concurrent schema runs can race on the same DDL. Stable and unique per service
// ("MKFM", marketflow migrate).
const migrationLockKey int64 = 0x4D4B_464D

func main() {
	_ = godotenv.Load()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("migrate: load config: %v", err)
	}

	// Prefer the direct PostgreSQL URL for migrations — it bypasses PgBouncer transaction mode,
	// which blocks Atlas advisory locks and DDL transactions. The entrypoint also points
	// POSTGRES_URL at the migrate URL, so this is belt-and-suspenders.
	dbCfg := cfg.Database
	if dbCfg.MigrateURL != "" {
		dbCfg.URL = dbCfg.MigrateURL
	}
	// One connection: the advisory lock and every migration statement share one session.
	dbCfg.MaxOpenConns = 1
	dbCfg.MaxIdleConns = 1

	ctx := context.Background()
	client, sqlDB, err := database.NewClient(ctx, dbCfg)
	if err != nil {
		log.Fatalf("migrate: open database: %v", err)
	}
	defer sqlDB.Close()
	defer client.Close()

	if _, err := sqlDB.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
		log.Fatalf("migrate: acquire migration lock: %v", err)
	}
	migrateErr := database.RunMigrations(ctx, client)
	if _, err := sqlDB.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", migrationLockKey); err != nil {
		log.Printf("migrate: release migration lock: %v", err)
	}
	if migrateErr != nil {
		log.Fatalf("migrate: run migrations: %v", migrateErr)
	}
	log.Println("migrate: migrations applied successfully")
}
