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

	ctx := context.Background()
	client, sqlDB, err := database.NewClient(ctx, dbCfg)
	if err != nil {
		log.Fatalf("migrate: open database: %v", err)
	}
	defer sqlDB.Close()
	defer client.Close()

	if err := database.RunMigrations(ctx, client); err != nil {
		log.Fatalf("migrate: run migrations: %v", err)
	}
	log.Println("migrate: migrations applied successfully")
}
