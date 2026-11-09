package knowledge

import (
	"context"
	"errors"
	"testing"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

func TestSuggestionIDIgnoresCaseSpacingAndPunctuation(t *testing.T) {
	tid := uuid.New()
	a := SuggestionID(tid, "Do you deliver to Kisumu?")
	if b := SuggestionID(tid, "  do you  DELIVER to kisumu "); a != b {
		t.Fatal("the same question made two ids")
	}
	if SuggestionID(uuid.New(), "Do you deliver to Kisumu?") == a {
		t.Fatal("two tenants share an id")
	}
}

func TestKeepSuggestionSurvivesAPortalSave(t *testing.T) {
	e := &ent.KnowledgeBaseEntry{Metadata: map[string]any{metaSuggested: true, metaHelpfulCount: 3}}
	got := keepSuggestion(e, map[string]any{"note": "x"})
	if got[metaSuggested] != true || got[metaHelpfulCount] != 3 || got["note"] != "x" {
		t.Fatalf("metadata = %+v", got)
	}
	if keepSuggestion(e, nil) != nil {
		t.Fatal("nil means metadata is not written")
	}
}

// A helpful answer becomes one pending suggestion that nothing searches, repeats count up, the
// normal list leaves it out, and approval queues it for embedding. A rejected one never returns.
func TestFAQSuggestionLifecycle(t *testing.T) {
	db, tid := openSyncDB(t)
	s := NewService(db, zap.NewNop())
	ctx := context.Background()

	if err := s.SuggestFAQ(ctx, tid, "Call me on 0712 345 678 about delivery", "Sure"); !errors.Is(err, ErrRefused) {
		t.Fatalf("a question with a phone number was kept: %v", err)
	}
	q := "Do you deliver to Kisumu?"
	for i := 0; i < 2; i++ {
		if err := s.SuggestFAQ(ctx, tid, q, "Yes, within a day."); err != nil {
			t.Fatal(err)
		}
	}
	id := SuggestionID(tid, q)
	row := db.KnowledgeBaseEntry.GetX(ctx, id)
	if !row.Embedded || row.ChunkCount != 0 || row.Category != "faq" || row.Metadata[metaHelpfulCount] != float64(2) || !suggested(row) {
		t.Fatalf("pending suggestion: embedded=%v chunks=%d meta=%+v", row.Embedded, row.ChunkCount, row.Metadata)
	}

	pending, n, err := s.ListEntries(ctx, ListEntriesInput{TenantID: tid, Suggested: true})
	if err != nil || n != 1 || pending[0].ID != id {
		t.Fatalf("pending list: %d %v", n, err)
	}
	if _, n, _ := s.ListEntries(ctx, ListEntriesInput{TenantID: tid}); n != 0 {
		t.Fatalf("the knowledge base list showed %d suggestions", n)
	}

	// An owner edit keeps it a suggestion and out of search.
	words := "Yes, within a day, KES 200."
	if _, err := s.UpdateEntry(ctx, tid, id, UpdateEntryInput{Content: &words, Metadata: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	row = db.KnowledgeBaseEntry.GetX(ctx, id)
	if !row.Embedded || !suggested(row) {
		t.Fatalf("an edit made the suggestion live: embedded=%v meta=%+v", row.Embedded, row.Metadata)
	}

	approved, err := s.ApproveSuggestion(ctx, tid, id)
	if err != nil || approved.Embedded || suggested(approved) || approved.Content != words {
		t.Fatalf("approve: %+v %v", approved, err)
	}
	if _, n, _ := s.ListEntries(ctx, ListEntriesInput{TenantID: tid}); n != 1 {
		t.Fatalf("an approved entry is in the knowledge base: %d", n)
	}
	if err := s.RejectSuggestion(ctx, tid, id); !errors.Is(err, ErrNotSuggestion) {
		t.Fatalf("rejecting an approved entry: %v", err)
	}

	// Rejected stays rejected, and the same question counts up without coming back.
	q2 := "Are you open on Sunday?"
	if err := s.SuggestFAQ(ctx, tid, q2, "No."); err != nil {
		t.Fatal(err)
	}
	id2 := SuggestionID(tid, q2)
	if err := s.RejectSuggestion(ctx, tid, id2); err != nil {
		t.Fatal(err)
	}
	if err := s.SuggestFAQ(ctx, tid, q2, "No."); err != nil {
		t.Fatal(err)
	}
	if _, n, _ := s.ListEntries(ctx, ListEntriesInput{TenantID: tid, Suggested: true}); n != 0 {
		t.Fatalf("a rejected question came back: %d pending", n)
	}
	if _, n, _ := s.ListEntries(ctx, ListEntriesInput{TenantID: tid}); n != 1 {
		t.Fatalf("a rejected question reached the knowledge base: %d", n)
	}
}
