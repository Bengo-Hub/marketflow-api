package creative

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent/knowledgebaseentry"
)

// The end card's price comes from the tenant's own catalogue when the offering has none, and is
// left empty when the catalogue has no price either.
func TestCatalogPrice(t *testing.T) {
	db := openDB(t)
	s := NewService(db, zap.NewNop())
	tid := newTenant(t, db)
	ctx := context.Background()
	// A knowledge base entry needs its tenant row.
	db.Tenant.Create().SetID(tid).SetName("Catalog price").SetSlug("cp-" + tid.String()[:8]).ExecX(ctx)
	t.Cleanup(func() {
		_, _ = db.KnowledgeBaseEntry.Delete().Where(knowledgebaseentry.TenantID(tid)).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})
	db.KnowledgeBaseEntry.Create().SetTenantID(tid).SetScope("tenant").SetCategory("product").
		SetTitle("Codevertex POS").SetContent("a till").SetProductPrice(2500).SaveX(ctx)
	if got := s.catalogPrice(ctx, tid, map[string]any{"name": "codevertex pos"}); got != "KES 2,500" {
		t.Fatalf("price %q", got)
	}
	if got := s.catalogPrice(ctx, tid, map[string]any{"name": "Digitika"}); got != "" {
		t.Fatalf("an unknown offering has no price, got %q", got)
	}
}
