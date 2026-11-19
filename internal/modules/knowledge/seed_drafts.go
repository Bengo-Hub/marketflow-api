package knowledge

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bengobox/marketflow-api/internal/ent/knowledgebaseentry"
	"github.com/google/uuid"
)

// Drafted entries (seed defaults, owner 2026-11-19): a FAQ entry written from the business's own
// sources (its website, its account record) when the knowledge base says nothing on that topic.
// It is kept exactly like a chat suggestion, pending until the owner approves it, so nothing
// searches it and the assistant never quotes it before the owner has read it. The metadata says
// where it came from and when.
const (
	// SuggestedFromSeed marks a suggestion drafted by the seed defaults run.
	SuggestedFromSeed = "seed_defaults"
	metaSourceURL     = "source_url"
	metaDraftedAt     = "drafted_at"
	metaDraftedFrom   = "drafted_from_source"
)

// TenantTitles returns the titles of every entry the tenant owns, pending and rejected
// suggestions included, so a topic check also sees what was already offered to the owner.
func (s *Service) TenantTitles(ctx context.Context, tenantID uuid.UUID) ([]string, error) {
	return s.client.KnowledgeBaseEntry.Query().
		Where(knowledgebaseentry.TenantID(tenantID)).
		Limit(MaxSyncedEntries + MaxPendingSuggestions).
		Select(knowledgebaseentry.FieldTitle).
		Strings(ctx)
}

// DraftFAQ keeps a drafted answer as a pending suggestion. It returns false when the question was
// already there (pending, approved or rejected), so running it again changes nothing.
func (s *Service) DraftFAQ(ctx context.Context, tenantID uuid.UUID, question, answer, source, sourceURL string) (bool, error) {
	question = strings.TrimSpace(strings.Join(strings.Fields(question), " "))
	answer = strings.TrimSpace(answer)
	if n := utf8.RuneCountInString(question); n < minQuestionLen || n > maxQuestionLen || answer == "" {
		return false, ErrRefused
	}
	if utf8.RuneCountInString(answer) > maxAnswerLen {
		answer = string([]rune(answer)[:maxAnswerLen])
	}
	id := SuggestionID(tenantID, question)
	exists, err := s.client.KnowledgeBaseEntry.Query().Where(knowledgebaseentry.ID(id)).Exist(ctx)
	if err != nil {
		return false, fmt.Errorf("check drafted faq: %w", err)
	}
	if exists {
		return false, nil
	}
	pending, err := s.client.KnowledgeBaseEntry.Query().
		Where(knowledgebaseentry.TenantID(tenantID), isSuggested()).Count(ctx)
	if err != nil {
		return false, fmt.Errorf("count pending suggestions: %w", err)
	}
	if pending >= MaxPendingSuggestions {
		return false, nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	meta := map[string]any{metaSuggested: true, metaHelpfulCount: 0, metaSuggestedAt: now, metaSuggestFrom: SuggestedFromSeed,
		metaDraftedFrom: source, metaDraftedAt: now}
	if sourceURL != "" {
		meta[metaSourceURL] = sourceURL
	}
	err = s.client.KnowledgeBaseEntry.Create().
		SetID(id).SetTenantID(tenantID).SetScope("tenant").SetCategory("faq").
		SetTitle(question).SetContent(answer).
		// Embedded with no chunks, like every suggestion: nothing searches it until approval.
		SetEmbedded(true).SetChunkCount(0).
		SetMetadata(meta).
		// A concurrent run that created it first wins; this one changes nothing.
		OnConflictColumns(knowledgebaseentry.FieldID).Ignore().
		Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("keep drafted faq: %w", err)
	}
	return true, nil
}
