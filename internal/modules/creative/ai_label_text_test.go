package creative

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// The on-screen "AI-generated" note is off unless a business turns it on (owner, 2026-11-25), and
// turning it on never switches the platforms' own AI flag.
func TestOnScreenAITextIsOffByDefault(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	tid := newTenant(t, db)
	db.Tenant.Create().SetID(tid).SetName("AI text").SetSlug("ai-text-" + tid.String()).ExecX(ctx)
	t.Cleanup(func() { _ = db.Tenant.DeleteOneID(tid).Exec(context.Background()) })
	for _, c := range []struct {
		meta       map[string]any
		text, flag bool
	}{
		{map[string]any{}, false, true},
		{map[string]any{"ai_label": "on"}, false, true},
		{map[string]any{"ai_label_text": "on"}, true, true},
		{map[string]any{"ai_label_text": "off"}, false, true},
		{map[string]any{"ai_label": "off", "ai_label_text": "on"}, false, false},
	} {
		if err := db.Tenant.UpdateOneID(tid).SetMetadata(c.meta).Exec(ctx); err != nil {
			t.Fatal(err)
		}
		if got := TenantAILabelText(ctx, db, tid); got != c.text {
			t.Errorf("%v: text %v", c.meta, got)
		}
		if got := TenantAILabel(ctx, db, tid); got != c.flag {
			t.Errorf("%v: platform flag %v", c.meta, got)
		}
	}
	if TenantAILabelText(ctx, db, uuid.New()) {
		t.Error("an unknown tenant shows the note")
	}
}
