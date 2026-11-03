package worker

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
)

// Kick never blocks, and many kicks before a tick fold into one pending tick.
func TestSchedulerKickNeverBlocks(t *testing.T) {
	s := NewScheduler(nil, nil, SchedulerConfig{}, zap.NewNop())
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			s.Kick()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Kick blocked")
	}
	if got := len(s.kick); got != 1 {
		t.Fatalf("pending kicks = %d, want 1", got)
	}
}

// WhatsApp is sent only with the lead's own consent tick.
func TestWhatsAppConsent(t *testing.T) {
	cases := []struct {
		meta map[string]any
		want bool
	}{
		{map[string]any{"whatsapp_consent": true}, true},
		{map[string]any{"whatsapp_consent": false}, false},
		{map[string]any{"whatsapp_consent": "yes"}, false}, // only a real boolean counts
		{nil, false},
	}
	for _, c := range cases {
		if got := whatsAppConsent(&ent.Lead{Metadata: c.meta}); got != c.want {
			t.Errorf("%v: got %v, want %v", c.meta, got, c.want)
		}
	}
}

// The first automated reply is written once into metadata, keeps the other keys, and a later
// step never moves it.
func TestRecordFirstReplyOnce(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db, "first-reply")
	l, err := db.Lead.Create().SetTenantID(tid).SetSource("website").SetLeadType("customer").
		SetMetadata(map[string]any{"utm": "x"}).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	if err := recordFirstReply(ctx, db, l.ID, first); err != nil {
		t.Fatal(err)
	}
	if err := recordFirstReply(ctx, db, l.ID, first.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := db.Lead.Get(ctx, l.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata["first_reply_at"] != first.Format(time.RFC3339) || got.Metadata["utm"] != "x" {
		t.Fatalf("metadata = %v", got.Metadata)
	}
	if got.ContactedAt != nil {
		t.Fatalf("contacted_at is the person's own status change and must stay unset: %v", got.ContactedAt)
	}
}
