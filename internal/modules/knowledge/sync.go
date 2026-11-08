package knowledge

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/knowledgebaseentry"
	"github.com/google/uuid"
)

// Knowledge base sync (Sprint 25, owner decision 2026-11-08). Entries that came from a business's
// own systems stay in step with them: upserted on change events and a nightly sweep, marked stale
// when the source item is gone. An entry the owner typed or edited is never overwritten; it gets a
// "may be outdated" flag instead.
//
// A synced entry's id is derived from (tenant, source, source id), so the upsert is one primary key
// operation with no JSON lookup, two deliveries of one event cannot make two rows, and a hand-typed
// entry (random id) can never collide with one. The tracking fields live in metadata.

// Sources of synced entries.
const (
	SourceInventory = "inventory"
	SourceWebsite   = "website"
)

// Metadata keys of a synced entry.
const (
	metaSource        = "source"
	metaSourceID      = "source_id"
	metaSourceHash    = "source_hash"
	metaSyncedAt      = "synced_at"
	metaStale         = "stale"
	metaStaleAt       = "stale_at"
	metaOwnerEdited   = "owner_edited"
	metaMayBeOutdated = "may_be_outdated"
)

// MaxSyncedEntries bounds one tenant's synced entries: embeddings are compute, and a catalogue of
// tens of thousands of items adds little to answers over the live catalogue search.
const MaxSyncedEntries = 2000

// syncNamespace seeds the derived entry ids. It must never change.
var syncNamespace = uuid.MustParse("6f1c9a52-7f0e-5a0b-9d43-2f6a1c7e8b10")

// SourceItem is one sellable thing as its owner describes it.
type SourceItem struct {
	TenantID    uuid.UUID
	Source      string
	SourceID    string
	Kind        string // GOODS, SERVICE, RECIPE, COURSE, PRODUCT
	Name        string
	Description string
	SKU         string
	// Price is the real customer price, from the nightly walk. Events leave it nil and the
	// entry keeps the price it has: an event's selling_price is only a guardrail ceiling.
	Price    *float64
	ImageURL string
	// Sellable is false for an item that is inactive, not for sale or otherwise not offered.
	Sellable bool
	// UpdateOnly refreshes an existing entry but creates none, for an event that cannot prove
	// the item is for sale (inventory's item.updated carries no not_for_sale).
	UpdateOnly bool
}

// Outcome says what a sync did with one item.
type Outcome string

const (
	Created   Outcome = "created"
	Updated   Outcome = "updated"
	Unchanged Outcome = "unchanged"
	MadeStale Outcome = "stale"
	Flagged   Outcome = "flagged"
	Skipped   Outcome = "skipped"
)

// Sync applies source items to the knowledge base. raw reaches the embeddings table, which Ent does
// not model.
type Sync struct {
	db  *ent.Client
	raw *sql.DB
	now func() time.Time
}

func NewSync(db *ent.Client, raw *sql.DB) *Sync { return &Sync{db: db, raw: raw, now: time.Now} }

// EntryID is the derived id of a synced entry.
func EntryID(tenantID uuid.UUID, source, sourceID string) uuid.UUID {
	return uuid.NewSHA1(syncNamespace, []byte(tenantID.String()+"|"+source+"|"+sourceID))
}

func categoryOf(kind string) string {
	switch strings.ToUpper(kind) {
	case "SERVICE":
		return "service"
	case "RECIPE":
		return "menu"
	case "COURSE":
		return "course"
	default:
		return "product"
	}
}

// render is the entry's text, built only from what every path agrees on (name, description,
// price), so an event and the nightly walk hash the same and an unchanged item is never re-embedded.
func render(it SourceItem) string {
	lines := []string{strings.TrimSpace(it.Name)}
	if d := strings.TrimSpace(it.Description); d != "" {
		lines = append(lines, d)
	}
	if it.Price != nil && *it.Price > 0 {
		lines = append(lines, fmt.Sprintf("Price: %.2f", *it.Price))
	}
	return strings.Join(lines, "\n")
}

func hashOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:12])
}

func copyMeta(in map[string]any) map[string]any {
	out := make(map[string]any, len(in)+6)
	for k, v := range in {
		out[k] = v
	}
	return out
}

// Apply upserts one item.
func (s *Sync) Apply(ctx context.Context, it SourceItem) (Outcome, error) {
	if it.SourceID == "" || strings.TrimSpace(it.Name) == "" {
		return Skipped, nil
	}
	id := EntryID(it.TenantID, it.Source, it.SourceID)
	row, err := s.db.KnowledgeBaseEntry.Get(ctx, id)
	if err != nil && !ent.IsNotFound(err) {
		return Skipped, err
	}
	if !it.Sellable {
		if row == nil {
			return Skipped, nil
		}
		return s.markStale(ctx, row)
	}
	if row != nil {
		if stale, _ := row.Metadata[metaStale].(bool); stale && it.UpdateOnly {
			// This event cannot prove the item is for sale again; the nightly walk can.
			return Unchanged, nil
		}
		if it.Price == nil {
			it.Price = row.ProductPrice
		}
	}
	content := render(it)
	hash := hashOf(content)
	if row != nil {
		return s.refresh(ctx, row, it, content, hash)
	}
	if it.UpdateOnly {
		return Skipped, nil
	}
	return s.create(ctx, id, it, content, hash)
}

func (s *Sync) refresh(ctx context.Context, row *ent.KnowledgeBaseEntry, it SourceItem, content, hash string) (Outcome, error) {
	meta := copyMeta(row.Metadata)
	stale, _ := meta[metaStale].(bool)
	same := meta[metaSourceHash] == hash
	if edited, _ := meta[metaOwnerEdited].(bool); edited {
		// The owner's words stand. Say once that the source moved on.
		if same {
			return Unchanged, nil
		}
		meta[metaSourceHash], meta[metaMayBeOutdated] = hash, true
		return Flagged, row.Update().SetMetadata(meta).Exec(ctx)
	}
	if same && !stale {
		return Unchanged, nil
	}
	delete(meta, metaStale)
	delete(meta, metaStaleAt)
	meta[metaSourceHash], meta[metaSyncedAt] = hash, s.now().UTC().Format(time.RFC3339)
	up := row.Update().SetTitle(strings.TrimSpace(it.Name)).SetContent(content).SetCategory(categoryOf(it.Kind)).
		SetMetadata(meta).SetEmbedded(false).SetChunkCount(0)
	if it.SKU != "" {
		up.SetProductCode(it.SKU)
	}
	if it.Price != nil {
		up.SetProductPrice(*it.Price)
	}
	if it.ImageURL != "" {
		up.SetProductImageURL(it.ImageURL)
	}
	return Updated, up.Exec(ctx)
}

func (s *Sync) create(ctx context.Context, id uuid.UUID, it SourceItem, content, hash string) (Outcome, error) {
	name := strings.TrimSpace(it.Name)
	// The owner already wrote about this item: flag theirs rather than add a rival entry.
	manual, err := s.db.KnowledgeBaseEntry.Query().
		Where(knowledgebaseentry.TenantID(it.TenantID), knowledgebaseentry.CategoryIn("product", "service", "course", "menu"),
			knowledgebaseentry.TitleEqualFold(name), func(sel *entsql.Selector) {
				sel.Where(entsql.ExprP(sel.C(knowledgebaseentry.FieldMetadata) + "->>'source' IS NULL"))
			}).
		First(ctx)
	if err != nil && !ent.IsNotFound(err) {
		return Skipped, err
	}
	if manual != nil {
		if flagged, _ := manual.Metadata[metaMayBeOutdated].(bool); flagged || !differs(manual, it) {
			return Unchanged, nil
		}
		meta := copyMeta(manual.Metadata)
		meta[metaMayBeOutdated] = true
		return Flagged, manual.Update().SetMetadata(meta).Exec(ctx)
	}
	n, err := s.syncedCount(ctx, it.TenantID)
	if err != nil {
		return Skipped, err
	}
	if n >= MaxSyncedEntries {
		return Skipped, nil
	}
	c := s.db.KnowledgeBaseEntry.Create().SetID(id).SetTenantID(it.TenantID).SetCategory(categoryOf(it.Kind)).
		SetTitle(name).SetContent(content).SetEmbedded(false).
		SetMetadata(map[string]any{metaSource: it.Source, metaSourceID: it.SourceID, metaSourceHash: hash,
			metaSyncedAt: s.now().UTC().Format(time.RFC3339)})
	if it.SKU != "" {
		c.SetProductCode(it.SKU)
	}
	if it.Price != nil {
		c.SetProductPrice(*it.Price)
	}
	if it.ImageURL != "" {
		c.SetProductImageURL(it.ImageURL)
	}
	// A second delivery of the same new item loses this race harmlessly.
	if err := c.OnConflictColumns(knowledgebaseentry.FieldID).Ignore().Exec(ctx); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Skipped, err
	}
	return Created, nil
}

// differs reports whether a hand-written entry disagrees with the source on price, the one fact
// that can be compared without reading prose.
func differs(e *ent.KnowledgeBaseEntry, it SourceItem) bool {
	return it.Price != nil && *it.Price > 0 && e.ProductPrice != nil && *e.ProductPrice != *it.Price
}

func (s *Sync) syncedCount(ctx context.Context, tenantID uuid.UUID) (int, error) {
	return s.db.KnowledgeBaseEntry.Query().
		Where(knowledgebaseentry.TenantID(tenantID), func(sel *entsql.Selector) {
			sel.Where(entsql.ExprP(sel.C(knowledgebaseentry.FieldMetadata) + "->>'source' IS NOT NULL"))
		}).Count(ctx)
}

// markStale takes an entry out of search but keeps it, so a restocked or re-listed item returns
// with its history. Embedded stays true with no chunks, meaning there is nothing left to embed.
// An entry the owner edited is flagged instead, because the words are theirs to keep or remove.
func (s *Sync) markStale(ctx context.Context, row *ent.KnowledgeBaseEntry) (Outcome, error) {
	meta := copyMeta(row.Metadata)
	if edited, _ := meta[metaOwnerEdited].(bool); edited {
		if flagged, _ := meta[metaMayBeOutdated].(bool); flagged {
			return Unchanged, nil
		}
		meta[metaMayBeOutdated] = true
		return Flagged, row.Update().SetMetadata(meta).Exec(ctx)
	}
	if stale, _ := meta[metaStale].(bool); stale {
		return Unchanged, nil
	}
	if s.raw != nil {
		if _, err := s.raw.ExecContext(ctx, `DELETE FROM knowledge_base_embeddings WHERE entry_id = $1`, row.ID); err != nil {
			return Skipped, fmt.Errorf("remove stale embeddings: %w", err)
		}
	}
	meta[metaStale], meta[metaStaleAt] = true, s.now().UTC().Format(time.RFC3339)
	// A changed source hash next time is what brings it back, so the hash is cleared.
	delete(meta, metaSourceHash)
	return MadeStale, row.Update().SetMetadata(meta).SetEmbedded(true).SetChunkCount(0).Exec(ctx)
}

// MarkMissing marks stale every entry of a source the finished walk did not see. It is only for a
// complete walk that saw at least one item: an empty answer from a struggling service must not
// take a whole catalogue out of search.
func (s *Sync) MarkMissing(ctx context.Context, tenantID uuid.UUID, source string, seen map[string]bool) (int, error) {
	if len(seen) == 0 {
		return 0, nil
	}
	rows, err := s.db.KnowledgeBaseEntry.Query().
		Where(knowledgebaseentry.TenantID(tenantID), func(sel *entsql.Selector) {
			sel.Where(entsql.P(func(b *entsql.Builder) {
				b.WriteString(sel.C(knowledgebaseentry.FieldMetadata) + "->>'source' = ").Arg(source)
			}))
		}).
		Limit(MaxSyncedEntries + 1).All(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rows {
		id, _ := r.Metadata[metaSourceID].(string)
		if id == "" || seen[id] {
			continue
		}
		out, err := s.markStale(ctx, r)
		if err != nil {
			return n, err
		}
		if out == MadeStale {
			n++
		}
	}
	return n, nil
}
