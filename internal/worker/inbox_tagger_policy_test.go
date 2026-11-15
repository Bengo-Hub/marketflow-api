package worker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/activity"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// A tenant's own bars decide tagging and the automatic answer; a buyer comment gets the message
// privately plus the public line, and falls back to a public reply when the private one is refused.
func TestInboxTenantPolicyAndPrivateReply(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db, "inbox-policy")
	t.Cleanup(func() { _, _ = db.Activity.Delete().Where(activity.TenantID(tid)).Exec(ctx) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"intent":"buyer_price","confidence":0.75,"injection":0.01}`))
	}))
	defer srv.Close()

	seed := func() uuid.UUID {
		return db.Activity.Create().SetTenantID(tid).SetEntityType("contact").SetEntityID(uuid.New()).
			SetActivityType("comment").SetDirection("inbound").SetBody("price?").SetSource("facebook").
			SetExternalRef("c-" + uuid.NewString()).SetPerformedAt(time.Now().UTC()).
			SetMetadata(map[string]any{"platform": "facebook", "intent_status": "pending", "post_id": "P1"}).SaveX(ctx).ID
	}
	var public, private []string
	refusePrivate := false
	tg := NewInboxTagger(db, srv.URL, "k", zap.NewNop())
	record := func(a *ent.Activity, text string) {
		db.Activity.Create().SetTenantID(tid).SetEntityType("contact").SetEntityID(a.EntityID).SetActivityType("dm").
			SetDirection("outbound").SetBody(text).SetPerformedAt(time.Now().UTC()).
			SetMetadata(map[string]any{"auto": true, "intent_status": "outbound"}).ExecX(ctx)
	}
	tg.reply = func(_ context.Context, _ uuid.UUID, a *ent.Activity, text string) error {
		public = append(public, text)
		record(a, text)
		return nil
	}
	tg.privateReply = func(_ context.Context, _ uuid.UUID, a *ent.Activity, text string) error {
		if refusePrivate {
			return errors.New("(#10) permission not granted")
		}
		private = append(private, text)
		record(a, text)
		return nil
	}

	// A stricter tagging bar (0.8) leaves a 0.75 answer untagged, and nothing is sent.
	db.Tenant.UpdateOneID(tid).SetMetadata(map[string]any{
		"inbox_auto_reply": true, "inbox_reply_text": "Prices: {{booking_link}}", "inbox_intent_threshold": 0.8,
	}).ExecX(ctx)
	strict := seed()
	if _, err := tg.tick(ctx); err != nil {
		t.Fatal(err)
	}
	if m := db.Activity.GetX(ctx, strict).Metadata; m["intent_status"] != "untagged" || m["intent_bar"] != 0.8 || len(public)+len(private) != 0 {
		t.Fatalf("strict bar: %v (public %v private %v)", m, public, private)
	}

	// Default bars: 0.75 is tagged and passes the 0.7 reply bar; the message goes privately and the
	// public line goes under the comment.
	db.Tenant.UpdateOneID(tid).SetMetadata(map[string]any{
		"inbox_auto_reply": true, "inbox_reply_text": "Prices: {{booking_link}}", "inbox_comment_prompt": "Check your messages",
	}).ExecX(ctx)
	first := seed()
	if _, err := tg.tick(ctx); err != nil {
		t.Fatal(err)
	}
	m := db.Activity.GetX(ctx, first).Metadata
	if len(private) != 1 || len(public) != 1 || public[0] != "Check your messages" || m["auto_reply"] != "sent" || m["auto_reply_private"] != true {
		t.Fatalf("private plus public line: %v (public %v private %v)", m, public, private)
	}

	// The private reply refused: the message goes as the public reply instead.
	refusePrivate = true
	second := seed()
	if _, err := tg.tick(ctx); err != nil {
		t.Fatal(err)
	}
	m = db.Activity.GetX(ctx, second).Metadata
	if len(public) != 2 || public[1] == "Check your messages" || m["auto_reply"] != "sent" || m["auto_reply_private_error"] == nil {
		t.Fatalf("fallback to public: %v (public %v)", m, public)
	}

	// A reply bar above the answer's confidence keeps the tag and sends nothing.
	db.Tenant.UpdateOneID(tid).SetMetadata(map[string]any{
		"inbox_auto_reply": true, "inbox_reply_text": "Prices: {{booking_link}}", "inbox_auto_reply_threshold": 0.9,
	}).ExecX(ctx)
	third := seed()
	if _, err := tg.tick(ctx); err != nil {
		t.Fatal(err)
	}
	if m := db.Activity.GetX(ctx, third).Metadata; m["intent_status"] != "tagged" || m["auto_reply"] != nil || len(public) != 2 {
		t.Fatalf("high reply bar: %v (public %v)", m, public)
	}
}
