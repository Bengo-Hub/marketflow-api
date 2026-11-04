package worker

import (
	"context"
	"testing"

	"github.com/bengobox/marketflow-api/internal/ent/campaign"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"go.uber.org/zap"
)

// The first ad click makes an attributed lead; a second click keeps the first click id; the
// lead is linked to the boost that ran the ad.
func TestWhatsAppReferralAttribution(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db, "ctwa")
	t.Cleanup(func() {
		_, _ = db.Lead.Delete().Where(lead.TenantID(tid)).Exec(ctx)
		_, _ = db.Campaign.Delete().Where(campaign.TenantID(tid)).Exec(ctx)
	})
	c := db.Campaign.Create().SetTenantID(tid).SetName("Boost").SetCampaignType("lead_gen").SetPlatform("meta").
		SetStatus("active").SetMetaAdID("ad-" + tid.String()[:8]).SaveX(ctx)
	h := NewWhatsAppReferralHandler(db, zap.NewNop())
	ref := waReferral{TenantID: tid.String(), From: "254712000111", ContactName: "Akinyi", CtwaClid: "CLICK1", AdID: *c.MetaAdID}
	if err := h.attribute(ctx, tid, ref); err != nil {
		t.Fatal(err)
	}
	ref.CtwaClid = "CLICK2"
	if err := h.attribute(ctx, tid, ref); err != nil {
		t.Fatal(err)
	}
	ls := db.Lead.Query().Where(lead.TenantID(tid)).AllX(ctx)
	if len(ls) != 1 {
		t.Fatalf("one lead per person, got %d", len(ls))
	}
	l := ls[0]
	if l.Metadata["ctwa_clid"] != "CLICK1" || l.Source != "ctwa" || l.CampaignID == nil || *l.CampaignID != c.ID {
		t.Fatalf("attribution: %+v %v", l.Metadata, l.CampaignID)
	}
	if _, ok := l.Metadata["whatsapp_consent"]; ok {
		t.Fatal("an ad chat is not marketing consent")
	}
}
