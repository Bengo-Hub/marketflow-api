package marketingos

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bengobox/marketflow-api/internal/modules/tenant/metastore"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// MarketSuggestionsKey holds the onboarding complaint search: what it searched, the themes and the
// similar businesses it found, or why it did not run. It runs once per tenant (owner decision
// 2026-11-08: automatic, 1 credit, skipped without credits). Nothing here shapes an ad until the
// owner adds a theme to customer_complaints or tracks a business as a competitor.
const MarketSuggestionsKey = "market_suggestions"

// MarketSuggestions is the stored result.
type MarketSuggestions struct {
	Query  string          `json:"query"`
	Status string          `json:"status"` // ready | no_credits
	At     string          `json:"at"`
	Themes json.RawMessage `json:"themes,omitempty"`
	Places json.RawMessage `json:"places,omitempty"`
}

// marketQuery turns the positioning into a search: its first sentence, up to 120 characters, which
// usually names the kind of business and the town ("Coding classes for kids in Kisumu").
func marketQuery(positioning string) string {
	s := strings.Join(strings.Fields(positioning), " ")
	if i := strings.IndexAny(s, ".!?\n"); i > 0 {
		s = s[:i]
	}
	if utf8.RuneCountInString(s) > 120 {
		s = string([]rune(s)[:120])
		if i := strings.LastIndex(s, " "); i > 60 {
			s = s[:i]
		}
	}
	return strings.TrimSpace(s)
}

// SuggestFromMarketIfNew runs the complaint search once, after the first brand context with a
// positioning is saved. It never fails the save: it runs after the reply.
func (h *Handler) SuggestFromMarketIfNew(ctx context.Context, tid uuid.UUID) {
	if h.Complaints == nil {
		return
	}
	t, err := h.svc.db.Tenant.Get(ctx, tid)
	if err != nil {
		return
	}
	if _, done := t.Metadata[MarketSuggestionsKey]; done {
		return
	}
	bc, err := h.svc.GetBrandContext(ctx, tid)
	if err != nil {
		return
	}
	query := marketQuery(bc.Positioning)
	if utf8.RuneCountInString(query) < 8 {
		return // not enough to search on yet; a later save tries again
	}
	country, _ := h.svc.tenantCountryCode(ctx, tid)
	out := MarketSuggestions{Query: query, At: time.Now().UTC().Format(time.RFC3339)}
	status, body, err := h.Complaints(ctx, tid, query, country)
	switch {
	case err != nil || (status != http.StatusOK && status != http.StatusPaymentRequired):
		// Not recorded, so the next brand context save tries again (for example once the Serper key
		// is configured). Nothing was charged: marketflow-ai refunds a failed search.
		h.logger.Warn("onboarding market search", zap.String("tenant_id", tid.String()), zap.Int("status", status), zap.Error(err))
		return
	case status == http.StatusPaymentRequired:
		out.Status = "no_credits"
	default:
		var rep struct {
			Themes json.RawMessage `json:"themes"`
			Places json.RawMessage `json:"places"`
		}
		if json.Unmarshal(body, &rep) != nil {
			return
		}
		out.Status, out.Themes, out.Places = "ready", rep.Themes, rep.Places
	}
	if err := metastore.SetKeys(ctx, h.svc.db, tid, map[string]any{MarketSuggestionsKey: out}); err != nil {
		h.logger.Warn("keep onboarding market search", zap.String("tenant_id", tid.String()), zap.Error(err))
	}
}

// readMarketSuggestions returns the stored result, or nil.
func readMarketSuggestions(meta map[string]any) *MarketSuggestions {
	raw, ok := meta[MarketSuggestionsKey]
	if !ok {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var out MarketSuggestions
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return &out
}
