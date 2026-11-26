package creative

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent/creativebrief"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/bengobox/marketflow-api/internal/ent/funnel"
	"github.com/bengobox/marketflow-api/internal/ent/pipeline"
	"github.com/bengobox/marketflow-api/internal/ent/pipelinestage"
	"github.com/bengobox/marketflow-api/internal/ent/serviceconfig"
	"github.com/bengobox/marketflow-api/internal/ent/shortlink"
	"github.com/bengobox/marketflow-api/internal/modules/funnels"
)

func TestBareURL(t *testing.T) {
	for in, want := range map[string]string{"https://www.Shop.example/": "shop.example", "shop.example/s/AbC?x=1#y": "shop.example/s/AbC",
		"http://shop.example/f/soap/": "shop.example/f/soap"} {
		if got := bareURL(in); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}

// The end card gets the ad's lead page once, in a short form, beside the contacts; never when the
// card shows it already, the page is archived, the tenant switched it off, or the ad is a pharmacy's
// medicine. The caption's contacts do not change.
func TestCardContactsCarryTheLeadPage(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	tid := uuid.New()
	db.Tenant.Create().SetID(tid).SetName("Card").SetSlug("card-" + tid.String()).ExecX(ctx)
	mk := func(title string, offering map[string]any) (*Renderer, func() []contact) {
		b := db.CreativeBrief.Create().SetTenantID(tid).SetTitle(title).SetOffering(offering).SaveX(ctx)
		v := db.CreativeVersion.Create().SetTenantID(tid).SetBriefID(b.ID).SetVersionNo(1).SetKind("video_ad").SetFormat("9:16").
			SetStatus(StatusScriptReady).SetCopy(map[string]any{"end_card": map[string]any{"headline": title}}).SaveX(ctx)
		k := &PostKit{Svc: NewService(db, zap.NewNop()).WithLeadFunnels(funnels.NewService(db, zap.NewNop()).ForAds()), Base: "https://marketflow.example.com"}
		r := NewRenderer(db, "", "", zap.NewNop()).WithCardLink(k)
		return r, func() []contact {
			return r.endCardWordsFor(ctx, v, b, placeInfo{Contacts: []contact{{Kind: "web", Value: "shop.example"}}}).Contacts
		}
	}
	t.Cleanup(func() {
		_, _ = db.ShortLink.Delete().Where(shortlink.TenantID(tid)).Exec(ctx)
		_, _ = db.ServiceConfig.Delete().Where(serviceconfig.TenantID(tid)).Exec(ctx)
		_, _ = db.PipelineStage.Delete().Where(pipelinestage.TenantID(tid)).Exec(ctx)
		_, _ = db.Pipeline.Delete().Where(pipeline.TenantID(tid)).Exec(ctx)
		_, _ = db.Funnel.Delete().Where(funnel.TenantID(tid)).Exec(ctx)
		_, _ = db.CreativeVersion.Delete().Where(creativeversion.TenantID(tid)).Exec(ctx)
		_, _ = db.CreativeBrief.Delete().Where(creativebrief.TenantID(tid)).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})
	_, short := mk("Soap", nil)
	got := short()
	f := db.Funnel.Query().Where(funnel.TenantID(tid)).OnlyX(ctx)
	if len(got) != 2 || got[0].Kind != "web" || got[1] != (contact{"page", "marketflow.example.com/f/" + f.Slug}) {
		t.Fatalf("contacts %+v", got)
	}
	r, long := mk("A very long headline about our handmade shea butter soap bars", nil)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); long() }()
	}
	wg.Wait()
	l := db.ShortLink.Query().Where(shortlink.TenantID(tid)).OnlyX(ctx)
	if got := long(); len(got) != 2 || got[1].Value != "marketflow.example.com/s/"+l.Code || !strings.Contains(l.TargetURL, "utm_source=end_card") {
		t.Fatalf("contacts %+v %s", got, l.TargetURL)
	}
	same := placeInfo{Contacts: []contact{{Kind: "web", Value: "https://www." + got[1].Value + "/"}}}
	soap := db.CreativeBrief.Query().Where(creativebrief.TenantID(tid), creativebrief.Title("Soap")).OnlyIDX(ctx)
	v := db.CreativeVersion.Query().Where(creativeversion.TenantID(tid), creativeversion.BriefID(soap)).OnlyX(ctx)
	if c := r.cardContacts(ctx, v, same); len(c) != 1 {
		t.Fatalf("the same page twice: %+v", c)
	}
	db.Funnel.UpdateOneID(f.ID).SetStatus("archived").ExecX(ctx)
	if got := short(); len(got) != 1 {
		t.Fatalf("an archived page: %+v", got)
	}
	db.Funnel.UpdateOneID(f.ID).SetStatus("published").ExecX(ctx)
	db.Tenant.UpdateOneID(tid).SetBusinessType(BusinessTypePharmacy).ExecX(ctx)
	_, med := mk("Panadol tablets", map[string]any{"name": "Panadol tablets 500mg"})
	if got := med(); len(got) != 1 {
		t.Fatalf("a medicine ad: %+v", got)
	}
	if got := short(); len(got) != 2 {
		t.Fatalf("a pharmacy's soap: %+v", got)
	}
	db.ServiceConfig.Create().SetTenantID(tid).SetConfigKey(CardLinkKey).SetConfigValue("off").SetConfigType("string").ExecX(ctx)
	if got := short(); len(got) != 1 {
		t.Fatalf("switched off: %+v", got)
	}
}
