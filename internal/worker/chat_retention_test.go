package worker

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"go.uber.org/zap"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/chatsession"
)

func TestChatRetentionPrunesOnlyExpiredSessions(t *testing.T) {
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", testWorkerPostgresDSN)
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		t.Skipf("local postgres unreachable: %v", err)
	}
	defer sqlDB.Close()
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, sqlDB)))
	defer client.Close()
	ctx := context.Background()

	tid := uuid.New()
	if _, err := client.Tenant.Create().SetID(tid).SetName("Retention " + tid.String()).SetSlug("retention-" + tid.String()).Save(ctx); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	// Cleanup touches only this test's own tenant id.
	t.Cleanup(func() {
		_, _ = client.ChatSession.Delete().Where(chatsession.TenantID(tid)).Exec(ctx)
		_ = client.Tenant.DeleteOneID(tid).Exec(ctx)
	})

	now := time.Now().UTC()
	day := 24 * time.Hour
	type row struct {
		name     string
		started  time.Duration // how long ago
		activity *time.Duration
		status   string
		ended    bool
		wantGone bool
	}
	ago := func(d time.Duration) *time.Duration { return &d }
	rows := []row{
		{"recent activity on an old session stays", 200 * day, ago(2 * day), "active", false, false},
		{"stale activity is pruned", 60 * day, ago(31 * day), "active", false, true},
		{"activity just inside the window stays", 40 * day, ago(29 * day), "active", false, false},
		{"legacy closed and old is pruned", 40 * day, nil, "closed", false, true},
		{"legacy ended and old is pruned", 40 * day, nil, "active", true, true},
		{"legacy open, 40 days, stays (could still be resumed)", 40 * day, nil, "active", false, false},
		{"legacy open beyond the 90-day grace is pruned", 91 * day, nil, "active", false, true},
		{"legacy closed but young stays", 10 * day, nil, "closed", false, false},
	}

	ids := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		b := client.ChatSession.Create().SetTenantID(tid).SetChannel("web_widget").SetStatus(r.status).
			SetStartedAt(now.Add(-r.started)).SetMessages([]map[string]any{})
		if r.activity != nil {
			b.SetLastActivityAt(now.Add(-*r.activity))
		}
		if r.ended {
			b.SetEndedAt(now.Add(-r.started + time.Hour))
		}
		s, err := b.Save(ctx)
		if err != nil {
			t.Fatalf("seed %q: %v", r.name, err)
		}
		ids[i] = s.ID
	}

	job := NewChatRetention(client, 30, zap.NewNop())
	if _, err := job.prune(ctx, now); err != nil {
		t.Fatalf("prune: %v", err)
	}
	for i, r := range rows {
		exists, err := client.ChatSession.Query().Where(chatsession.ID(ids[i])).Exist(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if gone := !exists; gone != r.wantGone {
			t.Errorf("%s: gone=%v, want %v", r.name, gone, r.wantGone)
		}
	}

	// A second run finds nothing left to prune among this tenant's rows.
	if _, err := job.prune(ctx, now); err != nil {
		t.Fatalf("second prune: %v", err)
	}
}

func TestChatRetentionDisabledWhenZero(t *testing.T) {
	job := NewChatRetention(nil, 0, zap.NewNop())
	done := make(chan struct{})
	go func() { job.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a disabled retention job must return immediately")
	}
}
