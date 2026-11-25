package creative

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/serviceconfig"
)

// The default cues reach a tenant in their country only; turning the cues off keeps the trade marks
// out of drawn scenes.
func TestLocalCuesFor(t *testing.T) {
	var d localCues
	if err := json.Unmarshal([]byte(DefaultLocalCues), &d); err != nil || len(d.Cues) == 0 || d.Marks["m-pesa"] == "" {
		t.Fatalf("default %+v %v", d, err)
	}
	db := openDB(t)
	ctx := context.Background()
	ke, ng := uuid.New(), uuid.New()
	for id, country := range map[uuid.UUID]string{ke: "KE", ng: "NG"} {
		db.Tenant.Create().SetID(id).SetName("Cue " + id.String()).SetSlug("cue-" + id.String()).SetCountry(country).SaveX(ctx)
		t.Cleanup(func() {
			_, _ = db.ServiceConfig.Delete().Where(serviceconfig.TenantID(id)).Exec(ctx)
			_ = db.Tenant.DeleteOneID(id).Exec(ctx)
		})
	}
	if c := localCuesFor(ctx, db, ng); len(c.Cues) != 0 || len(c.Marks) == 0 {
		t.Fatalf("another country %+v", c)
	}
	db.ServiceConfig.Create().SetTenantID(ke).SetConfigKey(LocalCuesKey).SetConfigValue(`{"enabled":false}`).SaveX(ctx)
	if c := localCuesFor(ctx, db, ke); c.Enabled || len(c.Cues) != len(d.Cues) || c.Marks["m-pesa"] == "" {
		t.Fatalf("switched off %+v", c)
	}
}

func TestCueWords(t *testing.T) {
	v := &ent.CreativeVersion{Metadata: map[string]any{"storyboard": map[string]any{"stills": []any{map[string]any{"scene_id": "s1", "cue": "LIPA HAPA"}, map[string]any{"scene_id": "s2"}}}}}
	if got := cueWords(v); len(got) != 1 || got["s1"] != "LIPA HAPA" {
		t.Fatalf("cue words %v", got)
	}
}
