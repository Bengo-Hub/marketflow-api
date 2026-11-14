package publishing

import (
	"context"
	"testing"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent/creativeasset"
	"github.com/bengobox/marketflow-api/internal/ent/creativebrief"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/bengobox/marketflow-api/internal/ent/publishjob"
	"github.com/google/uuid"
)

// Owner 2026-11-14: the AI label is the owner's choice for every post. The autopilot's jobs carry
// ai_generated as the setting says, and the Studio's own posts read the same setting.
func TestAILabelReachesThePublishJob(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	s := &EntStore{DB: db}
	tid := uuid.New()
	db.Tenant.Create().SetID(tid).SetName("label " + tid.String()).SetSlug("label-" + tid.String()).ExecX(ctx)
	t.Cleanup(func() {
		_, _ = db.PublishJob.Delete().Where(publishjob.TenantIDEQ(tid)).Exec(ctx)
		_, _ = db.CreativeAsset.Delete().Where(creativeasset.TenantID(tid)).Exec(ctx)
		_, _ = db.CreativeVersion.Delete().Where(creativeversion.TenantID(tid)).Exec(ctx)
		_, _ = db.CreativeBrief.Delete().Where(creativebrief.TenantID(tid)).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})
	if !s.Enabled(ctx, Facebook) {
		row := db.ProviderSetting.Create().SetTenantID("platform").SetProviderType("publishing").SetProviderName("publishing").
			SetKey(Facebook + "_enabled").SetValue("true").SetIsActive(true).SaveX(ctx)
		t.Cleanup(func() { _ = db.ProviderSetting.DeleteOneID(row.ID).Exec(ctx) })
	}
	page := db.ConnectedAccount.Create().SetTenantID(tid).SetPlatform("meta").SetAccountType("facebook_page").SetExternalID("p_" + tid.String()[:8]).
		SetDisplayName("Shea Co").SetStatus("active").SetScopes([]string{"pages_show_list", "pages_read_engagement", "pages_manage_posts"}).SaveX(ctx)
	t.Cleanup(func() { _ = db.ConnectedAccount.DeleteOneID(page.ID).Exec(ctx) })

	video := func() (uuid.UUID, uuid.UUID) {
		b := db.CreativeBrief.Create().SetTenantID(tid).SetTitle("t").SaveX(ctx)
		v := db.CreativeVersion.Create().SetTenantID(tid).SetBriefID(b.ID).SetVersionNo(1).SetKind("video_ad").SetFormat("1080x1920").SetStatus("ready").SaveX(ctx)
		a := db.CreativeAsset.Create().SetTenantID(tid).SetVersionID(v.ID).SetKind("video").SetStorageKey("k/" + v.ID.String()).
			SetURL("https://media.example.com/v.mp4").SetDurationMs(20000).SetWidth(1080).SetHeight(1920).SaveX(ctx)
		return v.ID, a.ID
	}
	schedule := func(noLabel bool) bool {
		t.Helper()
		vid, aid := video()
		out, err := s.ScheduleForVeto(ctx, VetoRequest{TenantID: tid, AssetID: aid, VersionID: vid, AgentID: uuid.New(),
			AccountIDs: []string{page.ID.String()}, Platforms: []string{Facebook}, Caption: "Shea butter", At: time.Now().Add(4 * time.Hour),
			NoAILabel: noLabel})
		if err != nil || len(out.Jobs) != 1 {
			t.Fatalf("schedule: %+v %v", out, err)
		}
		job := db.PublishJob.GetX(ctx, uuid.MustParse(out.Jobs[0].ID))
		on, ok := job.Metadata["ai_generated"].(bool)
		if !ok {
			t.Fatalf("ai_generated is always written: %v", job.Metadata)
		}
		return on
	}
	if !schedule(false) {
		t.Fatal("the label is on by default")
	}
	if schedule(true) {
		t.Fatal("the owner turned the label off")
	}

	// The Studio's own posts read the tenant setting.
	if !aiLabelOn(ctx, db, tid) {
		t.Fatal("unset means on")
	}
	db.Tenant.UpdateOneID(tid).SetMetadata(map[string]any{aiLabelKey: "off"}).ExecX(ctx)
	if aiLabelOn(ctx, db, tid) {
		t.Fatal("off is off")
	}
	if !aiLabelOn(ctx, db, uuid.New()) {
		t.Fatal("an unreadable tenant keeps the label")
	}
}
