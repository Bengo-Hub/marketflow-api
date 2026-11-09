package knowledge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/knowledgebaseentry"
	"github.com/bengobox/marketflow-api/internal/modules/learning"
	"github.com/google/uuid"
)

// FAQ suggestions (Sprint 25): an answer someone rated helpful in a chat is kept as a suggested FAQ
// entry for the owner to approve. A suggestion is stored like a stale entry (embedded with no
// chunks), so neither the embed sweep nor any search reads it; approving it queues it for
// embedding. The question is the dedupe key, so the same question asked again only counts up.
const (
	metaSuggested    = "suggested"
	metaHelpfulCount = "helpful_count"
	metaSuggestedAt  = "suggested_at"
	metaSuggestFrom  = "suggested_from"

	// MaxPendingSuggestions bounds one tenant's unreviewed suggestions; past it, new questions are
	// dropped and repeats of pending ones still count.
	MaxPendingSuggestions = 100
	maxQuestionLen        = 300
	maxAnswerLen          = 4000
	minQuestionLen        = 8
)

// ErrRefused is a question the store will not keep (personal details, too short or too long).
var ErrRefused = errors.New("question refused")

// suggestionNS separates suggestion ids from synced ones (sync.go EntryID uses its own space).
var suggestionNS = uuid.MustParse("6b2f5f0e-7d3a-4c1e-9a55-1f0c7e2b9d41")

// normalQuestion is the dedupe form: lowercase words, single spaces, no closing punctuation.
func normalQuestion(q string) string {
	q = strings.ToLower(strings.Join(strings.Fields(q), " "))
	return strings.TrimRight(q, " ?.!")
}

// SuggestionID is the entry id for a tenant's question, so a repeat lands on the same row.
func SuggestionID(tenantID uuid.UUID, question string) uuid.UUID {
	return uuid.NewSHA1(suggestionNS, []byte(tenantID.String()+"|"+normalQuestion(question)))
}

// SuggestFAQ keeps a helpful answer as a suggestion, or counts one more helpful rating for it.
// An entry the owner already approved only counts up; its words are never replaced.
func (s *Service) SuggestFAQ(ctx context.Context, tenantID uuid.UUID, question, answer string) error {
	question = strings.TrimSpace(strings.Join(strings.Fields(question), " "))
	answer = strings.TrimSpace(answer)
	if n := utf8.RuneCountInString(question); n < minQuestionLen || n > maxQuestionLen || answer == "" || learning.HasPersonalDetails(question) {
		return ErrRefused
	}
	if utf8.RuneCountInString(answer) > maxAnswerLen {
		answer = string([]rune(answer)[:maxAnswerLen])
	}
	id := SuggestionID(tenantID, question)
	now := time.Now().UTC().Format(time.RFC3339)

	// A repeat counts up in one statement, whether pending or approved.
	n, err := s.client.KnowledgeBaseEntry.Update().
		Where(knowledgebaseentry.ID(id), knowledgebaseentry.TenantID(tenantID)).
		Modify(func(u *entsql.UpdateBuilder) {
			u.Set(knowledgebaseentry.FieldMetadata, entsql.ExprFunc(func(b *entsql.Builder) {
				b.WriteString("jsonb_set(COALESCE(" + knowledgebaseentry.FieldMetadata + ", '{}'::jsonb), '{" + metaHelpfulCount + "}', to_jsonb(COALESCE((" +
					knowledgebaseentry.FieldMetadata + "->>'" + metaHelpfulCount + "')::int, 0) + 1))")
			}))
		}).Save(ctx)
	if err != nil {
		return fmt.Errorf("count faq suggestion: %w", err)
	}
	if n > 0 {
		return nil
	}

	pending, err := s.client.KnowledgeBaseEntry.Query().
		Where(knowledgebaseentry.TenantID(tenantID), isSuggested()).Count(ctx)
	if err != nil {
		return fmt.Errorf("count pending suggestions: %w", err)
	}
	if pending >= MaxPendingSuggestions {
		return nil
	}
	err = s.client.KnowledgeBaseEntry.Create().
		SetID(id).SetTenantID(tenantID).SetScope("tenant").SetCategory("faq").
		SetTitle(question).SetContent(answer).
		// Embedded with no chunks: nothing searches it and the sweep leaves it until approval.
		SetEmbedded(true).SetChunkCount(0).
		SetMetadata(map[string]any{metaSuggested: true, metaHelpfulCount: 1, metaSuggestedAt: now, metaSuggestFrom: "chat"}).
		OnConflictColumns(knowledgebaseentry.FieldID).Ignore().
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("keep faq suggestion: %w", err)
	}
	return nil
}

// ApproveSuggestion makes a suggestion a normal FAQ entry and queues it for embedding.
func (s *Service) ApproveSuggestion(ctx context.Context, tenantID, entryID uuid.UUID) (*ent.KnowledgeBaseEntry, error) {
	e, err := s.client.KnowledgeBaseEntry.Query().
		Where(knowledgebaseentry.ID(entryID), knowledgebaseentry.TenantID(tenantID)).Only(ctx)
	if err != nil {
		return nil, err
	}
	if !suggested(e) {
		return e, nil
	}
	meta := map[string]any{}
	for k, v := range e.Metadata {
		meta[k] = v
	}
	delete(meta, metaSuggested)
	meta["approved_at"] = time.Now().UTC().Format(time.RFC3339)
	return e.Update().SetMetadata(meta).SetEmbedded(false).SetChunkCount(0).Save(ctx)
}

// suggestionKeys only SuggestFAQ and ApproveSuggestion write; a portal save cannot drop them, so an
// edited suggestion stays a suggestion until the owner approves it.
var suggestionKeys = []string{metaSuggested, metaHelpfulCount, metaSuggestedAt, metaSuggestFrom}

// keepSuggestion carries the suggestion keys through an update's new metadata (nil means the
// metadata is not being written).
func keepSuggestion(e *ent.KnowledgeBaseEntry, meta map[string]any) map[string]any {
	if meta == nil || !suggested(e) {
		return meta
	}
	for _, k := range suggestionKeys {
		if v, ok := e.Metadata[k]; ok {
			meta[k] = v
		}
	}
	return meta
}

func suggested(e *ent.KnowledgeBaseEntry) bool {
	v, _ := e.Metadata[metaSuggested].(bool)
	return v
}

// rejectedValue marks a suggestion the owner turned down. The row stays (embedded, no chunks) so
// the same question rated helpful again only counts up and never comes back as a suggestion.
const rejectedValue = "rejected"

// RejectSuggestion turns a suggestion down for good.
func (s *Service) RejectSuggestion(ctx context.Context, tenantID, entryID uuid.UUID) error {
	e, err := s.client.KnowledgeBaseEntry.Query().
		Where(knowledgebaseentry.ID(entryID), knowledgebaseentry.TenantID(tenantID)).Only(ctx)
	if err != nil {
		return err
	}
	if !suggested(e) {
		return ErrNotSuggestion
	}
	meta := map[string]any{}
	for k, v := range e.Metadata {
		meta[k] = v
	}
	meta[metaSuggested] = rejectedValue
	meta["rejected_at"] = time.Now().UTC().Format(time.RFC3339)
	return e.Update().SetMetadata(meta).Exec(ctx)
}

// ErrNotSuggestion is an entry that is not a pending suggestion.
var ErrNotSuggestion = errors.New("this entry is not a suggestion")

// isSuggested matches pending suggestions.
func isSuggested() func(*entsql.Selector) {
	return func(sel *entsql.Selector) {
		sel.Where(entsql.ExprP("COALESCE(" + sel.C(knowledgebaseentry.FieldMetadata) + "->>'" + metaSuggested + "', '') = 'true'"))
	}
}

// notSuggested matches the knowledge base proper: no pending and no rejected suggestions.
func notSuggested() func(*entsql.Selector) {
	return func(sel *entsql.Selector) {
		sel.Where(entsql.ExprP("COALESCE(" + sel.C(knowledgebaseentry.FieldMetadata) + "->>'" + metaSuggested + "', '') NOT IN ('true', '" + rejectedValue + "')"))
	}
}
