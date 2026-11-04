package worker

import (
	"context"
	"testing"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/campaign"
	"github.com/bengobox/marketflow-api/internal/ent/creativebrief"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

type fakeStatus struct{ calls []string }

func (f *fakeStatus) SetStatus(_ context.Context, _, id, status string) error {
	f.calls = append(f.calls, id+":"+status)
	return nil
}

// Out of stock pauses the item's running boost; back in stock resumes it, but never a boost the
// owner paused by hand.
func TestStockPausesAndResumesBoosts(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db, "stockads")
	t.Cleanup(func() {
		ids, _ := db.CreativeBrief.Query().Where(creativebrief.TenantID(tid)).IDs(ctx)
		_, _ = db.Campaign.Delete().Where(campaign.TenantID(tid)).Exec(ctx)
		_, _ = db.CreativeVersion.Delete().Where(creativeversion.BriefIDIn(ids...)).Exec(ctx)
		_, _ = db.CreativeBrief.Delete().Where(creativebrief.TenantID(tid)).Exec(ctx)
	})
	item := uuid.NewString()
	brief := db.CreativeBrief.Create().SetTenantID(tid).SetTitle("Blender").
		SetOffering(map[string]any{"name": "Blender", "source_id": item}).SaveX(ctx)
	ver := db.CreativeVersion.Create().SetTenantID(tid).SetBriefID(brief.ID).SetVersionNo(1).SetFormat("1080x1920").SetStatus("approved").SaveX(ctx)
	mk := func(name, status string) *ent.Campaign {
		return db.Campaign.Create().SetTenantID(tid).SetName(name).SetCampaignType("lead_gen").SetPlatform("meta").SetStatus(status).
			SetCreativeVersionID(ver.ID).SetMetaCampaignID("mc-" + name).SaveX(ctx)
	}
	running := mk("running", "active")
	manual := mk("manual", "paused")

	fake := &fakeStatus{}
	ops := &BoostOps{db: db, meta: fake, logger: zap.NewNop(),
		token: func(context.Context, *ent.Campaign) (string, bool) { return "t", true }}
	out := &StockOutHandler{db: db, ops: ops, logger: zap.NewNop()}
	back := &StockBackHandler{db: db, ops: ops, logger: zap.NewNop(), now: time.Now}

	// affects_availability false is only an alert: the ads keep running.
	alert := []byte(`{"tenant_id":"` + tid.String() + `","item_id":"` + item + `","affects_availability":false}`)
	if err := out.Handle(ctx, fakeMsg{data: alert}); err != nil {
		t.Fatal(err)
	}
	if db.Campaign.GetX(ctx, running.ID).Status != "active" || len(fake.calls) != 0 {
		t.Fatal("an alert-only stock event must not pause the ads")
	}
	vs, _ := out.versionsForItem(ctx, tid, item)
	if len(vs) != 1 || vs[0] != ver.ID {
		t.Fatalf("versions for the item: %v", vs)
	}
	real := []byte(`{"tenant_id":"` + tid.String() + `","item_id":"` + item + `","name":"Blender","affects_availability":true}`)
	if err := out.Handle(ctx, fakeMsg{data: real}); err != nil {
		t.Fatal(err)
	}
	got := db.Campaign.GetX(ctx, running.ID)
	if got.Status != "paused" || got.Metadata[stockPausedKey] != item || len(fake.calls) != 1 || fake.calls[0] != "mc-running:PAUSED" {
		t.Fatalf("pause: %+v %v", got.Metadata, fake.calls)
	}

	rows := db.Campaign.Query().Where(campaign.TenantIDEQ(tid), campaign.StatusEQ("paused")).AllX(ctx)
	if len(rows) != 2 {
		t.Fatalf("both are paused now: %d", len(rows))
	}
	body := []byte(`{"tenant_id":"` + tid.String() + `","item_id":"` + item + `","quantity_after":5}`)
	if err := back.Handle(ctx, fakeMsg{data: body}); err != nil {
		t.Fatal(err)
	}
	if db.Campaign.GetX(ctx, running.ID).Status != "active" {
		t.Fatal("the stock-paused boost resumes")
	}
	if db.Campaign.GetX(ctx, manual.ID).Status != "paused" {
		t.Fatal("a boost the owner paused by hand stays paused")
	}
	if meta := db.Campaign.GetX(ctx, running.ID).Metadata; meta[stockPausedKey] != nil {
		t.Fatalf("the mark is cleared: %v", meta)
	}
}
