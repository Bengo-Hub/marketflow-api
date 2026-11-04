package custom_agents

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/customagent"
)

// The store lists platform templates by industry, Hire makes a switched-off copy that points
// back at the template, and template rows never show in the platform tenant's own agent list.
func TestAgentTemplatesStoreAndHire(t *testing.T) {
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", testCustomAgentsPostgresDSN)
	if err != nil || sqlDB.Ping() != nil {
		t.Skip("local postgres unreachable")
	}
	db := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, sqlDB)))
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	pid, tid := uuid.New(), uuid.New()
	slug := "plat-" + pid.String()[:8]
	db.Tenant.Create().SetID(pid).SetName("Platform " + slug).SetSlug(slug).ExecX(ctx)
	db.Tenant.Create().SetID(tid).SetName("Shop " + tid.String()).SetSlug("shop-" + tid.String()).ExecX(ctx)
	t.Cleanup(func() {
		for _, id := range []uuid.UUID{pid, tid} {
			_, _ = db.CustomAgent.Delete().Where(customagent.TenantID(id)).Exec(ctx)
			_ = db.Tenant.DeleteOneID(id).Exec(ctx)
		}
	})
	tpl := db.CustomAgent.Create().SetTenantID(pid).SetName("Weekly promo ads").SetTriggerType("cron").
		SetMetadata(map[string]any{"kind": KindAdAutopilot, "template": map[string]any{
			"category": "ads", "industries": []string{"retail", "beauty"}, "description": "A promo ad every week", "version": 2}}).SaveX(ctx)
	db.CustomAgent.Create().SetTenantID(pid).SetName("Own agent").SetTriggerType("manual").ExecX(ctx)

	s := NewService(db, zap.NewNop(), WithPlatformTenant(slug))
	all, err := s.ListTemplates(ctx, "")
	if err != nil || len(all) != 1 || all[0].Version != 2 || all[0].Kind != KindAdAutopilot {
		t.Fatalf("store: %+v %v", all, err)
	}
	if got, _ := s.ListTemplates(ctx, "Beauty"); len(got) != 1 {
		t.Fatal("industry filter is case-insensitive")
	}
	if got, _ := s.ListTemplates(ctx, "schools"); len(got) != 0 {
		t.Fatal("another industry must not match")
	}
	own, _ := s.List(ctx, pid)
	if len(own) != 1 || own[0].Name != "Own agent" {
		t.Fatalf("templates must not show as the platform's own agents: %v", own)
	}

	hired, err := s.Hire(ctx, tid, tpl.ID)
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := hired.Metadata["template_ref"].(map[string]any)
	if hired.IsActive || hired.TenantID != tid || ref["id"] != tpl.ID.String() || hired.Metadata["template"] != nil || hired.Metadata["kind"] != KindAdAutopilot {
		t.Fatalf("hired copy: %+v", hired)
	}
	if _, err := s.Hire(ctx, tid, hired.ID); err != ErrNotTemplate {
		t.Fatalf("a tenant agent is not a template: %v", err)
	}
}
