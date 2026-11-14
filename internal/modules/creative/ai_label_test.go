package creative

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bengobox/marketflow-api/internal/ent"
	"go.uber.org/zap"
)

// Owner 2026-11-14: the AI label is the owner's choice; anything but "off" keeps it.
func TestAILabelOn(t *testing.T) {
	cases := []struct {
		meta map[string]any
		want bool
	}{
		{nil, true},
		{map[string]any{AILabelKey: "on"}, true},
		{map[string]any{AILabelKey: "off"}, false},
		{map[string]any{AILabelKey: " OFF "}, false},
		{map[string]any{AILabelKey: true}, true},
	}
	for _, c := range cases {
		if got := AILabelOn(&ent.Tenant{Metadata: c.meta}); got != c.want {
			t.Errorf("%v: got %v", c.meta, got)
		}
	}
	if !AILabelOn(nil) {
		t.Error("no tenant keeps the label")
	}
}

// Every ad render call carries the tenant's choice, sent even when false.
func TestAdCallCarriesTheAILabel(t *testing.T) {
	db := openDB(t)
	tid := newTenant(t, db)
	ctx := context.Background()
	db.Tenant.Create().SetID(tid).SetName("Label Co").SetSlug("ai-label-" + tid.String()).ExecX(ctx)
	t.Cleanup(func() { _ = db.Tenant.DeleteOneID(tid).Exec(context.Background()) })
	b := newBrief(t, NewService(db, zap.NewNop()), tid)
	v := db.CreativeVersion.Create().SetTenantID(tid).SetBriefID(b.ID).SetVersionNo(1).SetKind(KindVideoAd).
		SetFormat("1080x1920").SetStatus(StatusReady).SaveX(ctx)
	r := NewRenderer(db, "", "", zap.NewNop())

	if call := r.adCallFor(ctx, v, VideoScript{}, false); !call.AILabel {
		t.Fatal("unset means the label stays on")
	}
	db.Tenant.UpdateOneID(tid).SetMetadata(map[string]any{AILabelKey: "off"}).ExecX(ctx)
	call := r.adCallFor(ctx, v, VideoScript{}, true)
	if call.AILabel {
		t.Fatal("the owner turned the label off")
	}
	raw, _ := json.Marshal(call)
	if !strings.Contains(string(raw), `"ai_label":false`) {
		t.Fatalf("ai_label must be sent when false: %s", raw)
	}
}
