package creative

import (
	"context"
	"testing"
	"time"

	"github.com/Bengo-Hub/httpware/objectstore"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/serviceconfig"
	"github.com/bengobox/marketflow-api/internal/modules/brandimages"
)

func TestStaticScenes(t *testing.T) {
	v := &ent.CreativeVersion{Metadata: map[string]any{"storyboard": map[string]any{"stills": []any{
		map[string]any{"scene_id": "s1", "screen": map[string]any{"id": "x"}},
		map[string]any{"scene_id": "s2"},
	}}}}
	if got := staticScenes(v); len(got) != 1 || got[0] != "s1" {
		t.Fatalf("static %v", got)
	}
	if staticScenes(&ent.CreativeVersion{}) != nil {
		t.Fatal("static scenes without a storyboard")
	}
}

// Screens are sent only when on and only the tenant's own; a tenant override turns them off.
func TestScreensFor(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	tid, other := uuid.New(), uuid.New()
	list := []any{
		map[string]any{"id": "shop", "key": brandimages.Prefix(tid) + "2026/11/shop.jpg"},
		map[string]any{"id": "dash", "key": brandimages.Prefix(tid) + "2026/11/dash.jpg", "kind": "screenshot", "device": "laptop"},
		map[string]any{"id": "foreign", "key": brandimages.Prefix(other) + "2026/11/x.jpg", "kind": "screenshot"},
	}
	if _, err := db.Tenant.Create().SetID(tid).SetName("Screens " + tid.String()).SetSlug("screens-" + tid.String()).
		SetMetadata(map[string]any{brandimages.MetadataKey: list}).Save(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.ServiceConfig.Delete().Where(serviceconfig.TenantID(tid)).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})
	store, err := objectstore.New("marketflow", objectstore.Config{LocalRoot: t.TempDir(), LocalURLBase: "http://localhost/media"})
	if err != nil {
		t.Fatal(err)
	}
	r := NewRenderer(db, "http://ai.test", "k", zap.NewNop()).WithBrandImages(brandimages.NewLinker(store, time.Hour))
	rules, shots := r.screensFor(ctx, tid)
	if rules == nil || !rules.Enabled || rules.MaxScenes != 2 || len(shots) != 1 || shots[0].ID != "dash" || shots[0].Device != "laptop" {
		t.Fatalf("rules %+v shots %+v", rules, shots)
	}
	if _, err := db.ServiceConfig.Create().SetTenantID(tid).SetConfigKey(ScreensKey).SetConfigValue(`{"enabled":false}`).Save(ctx); err != nil {
		t.Fatal(err)
	}
	if rules, shots := r.screensFor(ctx, tid); rules != nil || shots != nil {
		t.Fatal("sent screens that are off")
	}
}
