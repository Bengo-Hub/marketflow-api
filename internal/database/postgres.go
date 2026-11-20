package database

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/schema"
	"github.com/bengobox/marketflow-api/internal/config"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/migrate"
	_ "github.com/jackc/pgx/v5/stdlib" // register pgx driver
)

// NewClient initialises an Ent client backed by PostgreSQL.
// Returns both the Ent client and the underlying *sql.DB for use by the outbox repository.
func NewClient(ctx context.Context, cfg config.DatabaseConfig) (*ent.Client, *sql.DB, error) {
	db, err := sql.Open("pgx", cfg.URL)
	if err != nil {
		return nil, nil, fmt.Errorf("open postgres connection: %w", err)
	}

	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	db.SetConnMaxIdleTime(1 * time.Minute)

	if err := db.PingContext(ctx); err != nil {
		return nil, nil, fmt.Errorf("ping postgres: %w", err)
	}

	drv := entsql.OpenDB(dialect.Postgres, db)
	client := ent.NewClient(ent.Driver(drv))
	return client, db, nil
}

// ApplyFixups runs idempotent DDL for tables Ent does not model, after RunMigrations and under
// the same advisory lock. Schema.Create never executes the versioned .sql files, so anything a
// raw table needs must be here.
//
// kb_embeddings_entry_chunk: the embedding upsert uses ON CONFLICT (entry_id, chunk_index), and
// the 2026-04-09 table had no unique index for it, so every write failed and no knowledge base
// entry had an embedding until 2026-11-20 (Vera answered nothing from the knowledge base).
func ApplyFixups(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS kb_embeddings_entry_chunk ON knowledge_base_embeddings (entry_id, chunk_index)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("fixup %q: %w", s, err)
		}
	}
	return nil
}

// RunMigrations executes Atlas versioned migrations.
// Falls back to Ent auto-migrate if no versioned migrations are available.
func RunMigrations(ctx context.Context, client *ent.Client) error {
	return client.Schema.Create(ctx,
		schema.WithDir(migrate.Dir),
	)
}
