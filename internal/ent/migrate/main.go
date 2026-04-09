//go:build ignore

package main

import (
	"context"
	"log"
	"os"
	"strings"

	"github.com/bengobox/marketflow-api/internal/ent/migrate"

	atlasmigrate "ariga.io/atlas/sql/migrate"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql/schema"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/lib/pq"
)

func main() {
	ctx := context.Background()
	dir, err := atlasmigrate.NewLocalDir("internal/ent/migrate/migrations")
	if err != nil {
		log.Fatalf("failed creating atlas migration directory: %v", err)
	}
	opts := []schema.MigrateOption{
		schema.WithDir(dir),
		schema.WithMigrationMode(schema.ModeReplay),
		schema.WithDialect(dialect.Postgres),
		schema.WithFormatter(atlasmigrate.DefaultFormatter),
	}
	if len(os.Args) != 2 {
		log.Fatalln("migration name is required. use: 'go run -mod=mod internal/ent/migrate/main.go <name>'")
	}

	dbURL := os.Getenv("POSTGRES_URL")
	if dbURL == "" {
		log.Fatalln("POSTGRES_URL is required for migration generation")
	}

	// Use a separate search_path (schema) for dev migration generation
	devURL := dbURL
	if strings.Contains(devURL, "?") {
		devURL += "&search_path=ent_dev"
	} else {
		devURL += "?search_path=ent_dev"
	}

	err = migrate.NamedDiff(ctx, devURL, os.Args[1], opts...)
	if err != nil {
		log.Fatalf("failed generating migration: %v", err)
	}
}
