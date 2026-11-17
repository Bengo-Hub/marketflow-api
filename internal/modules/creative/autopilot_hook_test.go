package creative

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Restricted categories and compliance findings of severity high or review hold a veto-window
// video for a person; a low finding does not.
func TestFlaggedForReview(t *testing.T) {
	clean := &ent.CreativeVersion{Metadata: map[string]any{"findings": []any{map[string]any{"severity": "low", "issue": "tone"}}}}
	if got := FlaggedForReview(clean); len(got) != 0 {
		t.Fatalf("a low finding must not hold: %v", got)
	}
	flagged := &ent.CreativeVersion{Metadata: map[string]any{
		"restricted_categories": []any{"health"},
		"findings":              []any{map[string]any{"severity": "review", "issue": "claims a cure"}, map[string]any{"severity": "high"}},
	}}
	got := FlaggedForReview(flagged)
	if len(got) != 3 || !strings.Contains(got[0], "health") || !strings.Contains(got[1], "claims a cure") {
		t.Fatalf("flags: %v", got)
	}

	// The marketing laws' review (marketflow-ai team/laws_review.go, 2026-11-17): an unproven
	// "best" claim comes back with severity review and holds the video for a person; the format
	// advice (no hook, a vague call to action) is low and does not. Urgency, scarcity or a "was"
	// price with no fact behind it never arrives here: the script is blocked in marketflow-ai.
	laws := &ent.CreativeVersion{Metadata: map[string]any{"findings": []any{
		map[string]any{"severity": "review", "issue": `marketing law (superlative): a "best" or "No. 1" claim the facts do not prove`},
		map[string]any{"severity": "low", "issue": "the call to action asks for more than one next step (call, whatsapp)"},
	}}}
	if got := FlaggedForReview(laws); len(got) != 1 || !strings.Contains(got[0], "marketing law (superlative)") {
		t.Fatalf("only the unproven superlative holds the video: %v", got)
	}
}

// The script writer may use a deadline only when the facts carry it. marketflow-ai's urgency check
// (compliance/data/business_types.yaml, marketing_laws) reads "Offer ends: <day> <Month> <year>",
// so the wording here is a contract between the two services.
func TestOfferingFactsCarryTheRealDeadline(t *testing.T) {
	got := offeringFacts(map[string]any{"price": 850.0, "promo_code": "SUN10", "ends_at": "2026-11-30T21:00:00Z", "description": "20% off. Weekend promo"})
	for _, want := range []string{"Price: KES 850", "Promotion code: SUN10", "Offer ends: 30 November 2026", "20% off"} {
		if !strings.Contains(got, want) {
			t.Fatalf("offering facts lack %q:\n%s", want, got)
		}
	}
	if strings.Contains(offeringFacts(map[string]any{"price": 850.0}), "ends") {
		t.Fatal("an offering without an end date must not carry one")
	}
}

// The agent's storyboard approval is kept apart from a person's; the veto window's approval
// records no user; a draft's files are kept for the window; the state write is a claim.
func TestAutopilotVersionSteps(t *testing.T) {
	db := openDB(t)
	s := NewService(db, zap.NewNop())
	tid := newTenant(t, db)
	ctx := context.Background()
	b := newBrief(t, s, tid)
	agentID := uuid.New()

	v, err := s.CreateVideoAd(ctx, tid, b.ID, VideoAdInput{Autopilot: map[string]any{"agent_id": agentID.String(), "state": "active"}})
	if err != nil {
		t.Fatal(err)
	}
	if !IsAutopilotVersion(v) {
		t.Fatalf("the run's marker must be in the insert: %v", v.Metadata)
	}
	stills := []any{map[string]any{"scene_id": "s1", "url": "https://example.com/s1.png", "source": "generated"}}
	db.CreativeVersion.UpdateOneID(v.ID).SetStatus(StatusScriptReady).
		SetCopy(map[string]any{"script": map[string]any{"scenes": []any{}}}).
		SetMetadata(map[string]any{"autopilot": map[string]any{"agent_id": agentID.String()}, "storyboard": map[string]any{"stills": stills}}).
		ExecX(ctx)

	got, err := s.ApproveStoryboardByAgent(ctx, tid, v.ID, agentID)
	if err != nil {
		t.Fatal(err)
	}
	sb := got.Metadata["storyboard"].(map[string]any)
	if sb["approved_by_kind"] != "agent" || sb["approved_by_agent"] != agentID.String() || sb["approved_by"] != nil || !storyboardApproved(got) {
		t.Fatalf("agent approval: %v", sb)
	}

	// A stale read loses the claim.
	stale := got
	if err := SetAutopilotState(ctx, db, stale, map[string]any{"state": "scheduled"}); err != nil {
		t.Fatal(err)
	}
	if err := SetAutopilotState(ctx, db, stale, map[string]any{"state": "held"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a second write on the same read must conflict: %v", err)
	}
	if ap := db.CreativeVersion.GetX(ctx, v.ID).Metadata["autopilot"].(map[string]any); ap["state"] != "scheduled" || ap["agent_id"] != agentID.String() {
		t.Fatalf("state merge: %v", ap)
	}

	// The finished draft: its file is kept for the window, then approved by the window ending.
	soon := time.Now().Add(time.Hour)
	db.CreativeVersion.UpdateOneID(v.ID).SetStatus(StatusReady).ExecX(ctx)
	a := db.CreativeAsset.Create().SetTenantID(tid).SetVersionID(v.ID).SetKind(AssetVideo).
		SetStorageKey("k/v.mp4").SetURL("https://media.example.com/v.mp4").SetExpiresAt(soon).SaveX(ctx)
	later := time.Now().Add(80 * time.Hour).Truncate(time.Second)
	if err := s.KeepForVeto(ctx, tid, v.ID, later); err != nil {
		t.Fatal(err)
	}
	if e := db.CreativeAsset.GetX(ctx, a.ID).ExpiresAt; e == nil || !e.Equal(later) {
		t.Fatalf("expiry not extended: %v", e)
	}
	if err := s.KeepForVeto(ctx, tid, v.ID, soon); err != nil || !db.CreativeAsset.GetX(ctx, a.ID).ExpiresAt.Equal(later) {
		t.Fatalf("KeepForVeto must never shorten an expiry: %v", err)
	}
	if va, err := s.VideoAsset(ctx, tid, v.ID); err != nil || va.ID != a.ID {
		t.Fatalf("video asset: %v", err)
	}
	approved, err := s.ApproveAfterVeto(ctx, tid, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	ev := approved.Metadata["approval"].(map[string]any)
	if approved.Status != StatusApproved || approved.ApprovedBy != nil || ev["channel"] != string(ApprovalVetoWindow) || ev["user_id"] != nil {
		t.Fatalf("veto-window approval: %+v %v", approved, ev)
	}
	if db.CreativeAsset.GetX(ctx, a.ID).ExpiresAt != nil {
		t.Fatal("an approved video's file is kept")
	}
	if again, err := s.ApproveAfterVeto(ctx, tid, v.ID); err != nil || again.Status != StatusApproved {
		t.Fatalf("approving twice is a no-op: %v", err)
	}
}
