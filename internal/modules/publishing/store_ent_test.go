package publishing

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/schema"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/migrate"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

const testPostgresDSN = "postgres://postgres:postgres@localhost:5432/marketflow?sslmode=disable"

// openTestDB applies the declarative schema (as cmd/migrate does) to local PG17, which also
// proves the publish_jobs table and its partial index create cleanly.
func openTestDB(t *testing.T) *ent.Client {
	t.Helper()
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", testPostgresDSN)
	if err != nil || sqlDB.Ping() != nil {
		t.Skip("local postgres unreachable")
	}
	db := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, sqlDB)))
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Schema.Create(context.Background(), schema.WithDir(migrate.Dir)); err != nil {
		t.Fatalf("schema create: %v", err)
	}
	return db
}

func seedJob(t *testing.T, db *ent.Client, status string, at time.Time) *ent.PublishJob {
	t.Helper()
	tid := uuid.New()
	if _, err := db.Tenant.Create().SetID(tid).SetName("pub " + tid.String()).SetSlug("pub-" + tid.String()).Save(context.Background()); err != nil {
		t.Fatal(err)
	}
	asset, account := uuid.New(), uuid.New()
	row, err := db.PublishJob.Create().
		SetTenantID(tid).SetAssetID(asset).SetConnectedAccountID(account).
		SetPlatform(YouTube).SetStatus(status).SetScheduledAt(at).
		SetDedupeKey(asset.String() + ":" + account.String()).
		SetMetadata(map[string]any{"ai_generated": true, "planned_at": at.Format(time.RFC3339)}).
		Save(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func TestEntStoreSaveMergesAndRespectsCancel(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	s := &EntStore{DB: db}
	row := seedJob(t, db, StatusUploading, time.Now().UTC())

	j := &Job{ID: row.ID.String(), Status: StatusUploading, Attempts: 1, Error: "timeout",
		ScheduledAt: time.Now().UTC().Add(time.Minute),
		State:       UploadState{SessionURI: "https://up/s1", Offset: 8 << 20, ChunkIndex: 1}}
	if err := s.Save(ctx, j); err != nil {
		t.Fatal(err)
	}
	got := db.PublishJob.GetX(ctx, row.ID)
	up, _ := got.Metadata["upload"].(map[string]any)
	if got.Metadata["ai_generated"] != true || got.Metadata["planned_at"] == nil {
		t.Fatalf("merge lost the API keys: %v", got.Metadata)
	}
	if up["session_uri"] != "https://up/s1" || up["offset"] != float64(8<<20) || got.Attempts != 1 || got.Error != "timeout" {
		t.Fatalf("upload state not saved: %v %+v", got.Metadata, got)
	}
	if got.ExternalID != "" {
		t.Fatalf("external id set too early: %q", got.ExternalID)
	}

	db.PublishJob.UpdateOneID(row.ID).SetStatus(StatusCancelled).ExecX(ctx)
	if err := s.Save(ctx, j); !errors.Is(err, ErrCancelled) {
		t.Fatalf("save over a cancel must stop the job, got %v", err)
	}
	if db.PublishJob.GetX(ctx, row.ID).Status != StatusCancelled {
		t.Fatal("the cancel was overwritten")
	}
}

func TestEntStoreClaimLeasesOnceAndFailsMissingAsset(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	s := &EntStore{DB: db}
	now := time.Now().UTC().Truncate(time.Microsecond)
	due := seedJob(t, db, StatusPending, now.Add(-time.Minute))
	future := seedJob(t, db, StatusPending, now.Add(time.Hour))

	// The asset does not exist, so the claimed job fails at load instead of looping.
	if _, err := s.Claim(ctx, now, 500); err != nil {
		t.Fatal(err)
	}
	got := db.PublishJob.GetX(ctx, due.ID)
	if got.Status != StatusFailed || got.Error == "" {
		t.Fatalf("a job with no video must fail: %+v", got)
	}
	if db.PublishJob.GetX(ctx, future.ID).Status != StatusPending {
		t.Fatal("a future job was claimed")
	}

	// A second worker racing the same row cannot claim it once the lease moved scheduled_at.
	race := seedJob(t, db, StatusPending, now.Add(-time.Minute))
	n := db.PublishJob.Update().Where(func(sel *entsql.Selector) {
		sel.Where(entsql.EQ(sel.C("id"), race.ID))
	}).SetScheduledAt(now.Add(ClaimLease)).SaveX(ctx)
	if n != 1 {
		t.Fatal("lease update")
	}
	jobs, err := s.Claim(ctx, now, 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if j.ID == race.ID.String() {
			t.Fatal("a leased job was claimed again")
		}
	}
}

func TestEntStoreSwitch(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	s := &EntStore{DB: db}
	key := "test" + uuid.NewString()[:8]
	if s.Enabled(ctx, key) {
		t.Fatal("no row must mean off")
	}
	db.ProviderSetting.Create().SetTenantID("platform").SetProviderType("publishing").SetProviderName("publishing").
		SetKey(key + "_enabled").SetValue("true").SetIsActive(true).ExecX(ctx)
	if !s.Enabled(ctx, key) {
		t.Fatal("the switch row must turn it on")
	}
}
