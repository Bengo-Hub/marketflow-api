package creative

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/funnel"
	"github.com/bengobox/marketflow-api/internal/ent/pipeline"
	"github.com/bengobox/marketflow-api/internal/ent/pipelinestage"
	"github.com/bengobox/marketflow-api/internal/modules/funnels"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Posting makes the ad's lead page and the tenant's pipeline once; two posts at once make one of
// each; the caption ends with the link while the page is published, and the owner's archived page
// gives no link. The page itself has the lead form.
func TestPostKitMakesTheLeadPageOnce(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	tid := uuid.New()
	db.Tenant.Create().SetID(tid).SetName("Kit").SetSlug("kit-" + tid.String()).SetMetadata(map[string]any{PrivacyURLKey: "https://kit.example/privacy"}).ExecX(ctx)
	b := db.CreativeBrief.Create().SetTenantID(tid).SetTitle("Soft soap").SaveX(ctx)
	v := db.CreativeVersion.Create().SetTenantID(tid).SetBriefID(b.ID).SetVersionNo(1).SetKind("video_ad").SetFormat("9:16").
		SetStatus("approved").SetCopy(map[string]any{"end_card": map[string]any{"headline": "Soft soap", "cta": "Order"}}).SaveX(ctx)
	t.Cleanup(func() {
		_, _ = db.PipelineStage.Delete().Where(pipelinestage.TenantID(tid)).Exec(ctx)
		_, _ = db.Pipeline.Delete().Where(pipeline.TenantID(tid)).Exec(ctx)
		_, _ = db.Funnel.Delete().Where(funnel.TenantID(tid)).Exec(ctx)
		_ = db.CreativeVersion.DeleteOneID(v.ID).Exec(ctx)
		_ = db.CreativeBrief.DeleteOneID(b.ID).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})
	k := &PostKit{Svc: NewService(db, zap.NewNop()).WithLeadFunnels(funnels.NewService(db, zap.NewNop()).ForAds()), Base: "https://app.example"}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); k.LeadPageOf(ctx, tid, v.ID) }()
	}
	wg.Wait()
	p, err := k.Preset(ctx, tid, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	f := db.Funnel.Query().Where(funnel.TenantID(tid)).OnlyX(ctx)
	if db.Pipeline.Query().Where(pipeline.TenantID(tid)).CountX(ctx) != 1 || db.PipelineStage.Query().Where(pipelinestage.TenantID(tid)).CountX(ctx) != 5 {
		t.Fatal("one pipeline with its five stages")
	}
	if f.Status != "published" || len(f.Steps) != 1 || f.Steps[0]["block_type"] != "form" {
		t.Fatalf("page %+v", f)
	}
	if !strings.HasPrefix(p.Caption, "Soft soap\nOrder\n\nhttps://app.example/f/"+f.Slug) || !strings.Contains(p.AdURL, "utm_medium=paid") ||
		p.PrivacyURL != "https://kit.example/privacy" || p.PrivacyPrompt != "" {
		t.Fatalf("preset %+v", p)
	}
	f.Update().SetStatus("archived").ExecX(ctx)
	if p, _ = k.Preset(ctx, tid, v.ID); p.LeadURL != "" || p.AdURL != "" || strings.Contains(p.Caption, "/f/") {
		t.Fatalf("a link to an archived page: %+v", p)
	}
	db.Tenant.UpdateOneID(tid).SetMetadata(map[string]any{}).ExecX(ctx)
	if p, _ = k.Preset(ctx, tid, v.ID); p.PrivacyURL != "" || p.PrivacyPrompt == "" {
		t.Fatalf("no policy must prompt, never borrow one: %+v", p)
	}
}

func TestEnsureLeadFunnelWithoutWiring(t *testing.T) {
	s := NewService(nil, zap.NewNop())
	if _, err := s.ensureLeadFunnel(context.Background(), &ent.CreativeBrief{ID: uuid.New()}); !errors.Is(err, ErrNoLeadFunnels) {
		t.Fatalf("err = %v", err)
	}
	// The create path never fails the ad for it.
	b := &ent.CreativeBrief{ID: uuid.New()}
	if got := s.attachLeadFunnel(context.Background(), b); got != b {
		t.Fatal("attach changed the ad without lead pages wired")
	}
}
