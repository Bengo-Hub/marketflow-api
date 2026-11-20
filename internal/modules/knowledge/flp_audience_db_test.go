package knowledge

import (
	"context"
	"testing"

	"github.com/bengobox/marketflow-api/internal/ent/knowledgebaseentry"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Forever platform rows reach Forever tenants only, retired platform rows never show, and the
// anchor tenant's own id does not let Forever rows through.
func TestListEntriesHidesFLPPlatformRowsFromOtherTenants(t *testing.T) {
	_, client, anchor := openPlatformDB(t)
	ctx := context.Background()

	flpTenant := uuid.New()
	client.Tenant.Create().SetID(flpTenant).SetName("FLP " + flpTenant.String()).SetSlug("flp-" + flpTenant.String()).SetBusinessType("flp").SaveX(ctx)
	t.Cleanup(func() { _ = client.Tenant.DeleteOneID(flpTenant).Exec(ctx) })

	mk := func(scope, title string, meta map[string]any) {
		client.KnowledgeBaseEntry.Create().SetTenantID(anchor).SetScope(scope).SetCategory("product").
			SetTitle(title).SetContent(title).SetMetadata(meta).SaveX(ctx)
	}
	tag := uuid.NewString()
	mk("platform", "Aloe gel "+tag, map[string]any{"source_kind": "flp_products_seed"})
	mk("platform", "Forever tagged "+tag, map[string]any{"business_type": "flp"})
	mk("platform", "Retired "+tag, map[string]any{"stale": true})
	mk("platform", "Generic "+tag, map[string]any{"source_kind": "platform_seed"})
	mk("tenant", "Own "+tag, nil)
	t.Cleanup(func() {
		_, _ = client.KnowledgeBaseEntry.Delete().Where(knowledgebaseentry.TitleContains(tag)).Exec(ctx)
	})

	svc := NewService(client, zap.NewNop())
	titles := func(tenant uuid.UUID) map[string]bool {
		rows, _, err := svc.ListEntries(ctx, ListEntriesInput{TenantID: tenant, Search: tag, Limit: 100})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		out := map[string]bool{}
		for _, r := range rows {
			out[r.Title] = true
		}
		return out
	}

	got := titles(anchor)
	for _, want := range []string{"Generic " + tag, "Own " + tag} {
		if !got[want] {
			t.Errorf("anchor tenant should see %q, got %v", want, got)
		}
	}
	for _, hidden := range []string{"Aloe gel " + tag, "Forever tagged " + tag, "Retired " + tag} {
		if got[hidden] {
			t.Errorf("anchor tenant must not see %q", hidden)
		}
	}

	got = titles(flpTenant)
	for _, want := range []string{"Aloe gel " + tag, "Forever tagged " + tag, "Generic " + tag} {
		if !got[want] {
			t.Errorf("FLP tenant should see %q, got %v", want, got)
		}
	}
	if got["Own "+tag] || got["Retired "+tag] {
		t.Errorf("FLP tenant saw another tenant's row or a retired row: %v", got)
	}
}
