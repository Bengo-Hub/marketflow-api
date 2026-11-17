package knowledge

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Platform knowledge base: the Codevertex entries (company, Power Suite products, pricing,
// Digitika Academy) that Vera answers from for every tenant (scope = platform).
//
// platform_kb.json in this package is the one source of that content. The seed binary
// (cmd/seed, run by scripts/entrypoint.sh on every API pod start) calls SyncPlatformKB, which
// makes the database match the file:
//   - an entry missing from the database is created;
//   - an entry whose words, tags or metadata differ is rewritten, its search vectors are
//     deleted and it is queued for embedding (embedded = false), so the worker's embed sweep
//     (worker/kb_sync.go) re-embeds it within a minute. A portal edit of a seeded entry counts
//     as a difference too: the file wins, so edit the file instead;
//   - an entry the seed created that is no longer in the file is retired the Sprint 25 way
//     (metadata.stale, embeddings removed), never deleted.
//
// A seeded entry's id is derived from its category and title, and its metadata carries
// source_kind = platform_seed. Only rows with that marker (plus the titles the old hard-coded
// seed created, see legacyPlatformTitles) are ever retired; tenant rows and hand-written
// platform rows are never touched.
//
// Several pods may run the seed at once. Creates are insert-or-ignore on the derived id, and
// every rewrite or retirement locks its row in a short transaction and re-checks it, so a second
// pod finds nothing left to do. Nothing here needs a session advisory lock, which PgBouncer
// cannot hold.

//go:embed platform_kb.json
var platformKBFile []byte

// Metadata keys written by the platform seed.
const (
	metaSourceKind = "source_kind"
	metaSeededAt   = "seeded_at"
	// metaSeedSource holds the file's own "source" field (where a fact was checked). The key
	// "source" means an entry synced from a business's system (sync.go), so it is renamed.
	metaSeedSource = "seed_source"

	// SourceKindPlatformSeed marks an entry created and owned by SyncPlatformKB.
	SourceKindPlatformSeed = "platform_seed"
)

// reservedSeedMetaKeys may not appear in a file entry's metadata: the seed writes them itself.
var reservedSeedMetaKeys = []string{metaSourceKind, metaSeededAt, metaSeedSource, metaStale, metaStaleAt}

// PlatformEntry is one entry of platform_kb.json.
type PlatformEntry struct {
	Scope    string         `json:"scope"`
	Category string         `json:"category"`
	Title    string         `json:"title"`
	Content  string         `json:"content"`
	Tags     []string       `json:"tags,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// PlatformEntries returns the platform knowledge base shipped in this binary.
func PlatformEntries() ([]PlatformEntry, error) {
	return parsePlatformKB(platformKBFile)
}

func parsePlatformKB(data []byte) ([]PlatformEntry, error) {
	var entries []PlatformEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parse platform knowledge base: %w", err)
	}
	if len(entries) == 0 {
		// An empty file would retire every seeded entry, so it is refused.
		return nil, errors.New("platform knowledge base is empty")
	}
	seen := make(map[uuid.UUID]bool, len(entries))
	for i, e := range entries {
		if e.Scope != "platform" {
			return nil, fmt.Errorf("platform knowledge base entry %d (%q): scope must be platform, got %q", i, e.Title, e.Scope)
		}
		if strings.TrimSpace(e.Category) == "" || strings.TrimSpace(e.Title) == "" || strings.TrimSpace(e.Content) == "" {
			return nil, fmt.Errorf("platform knowledge base entry %d (%q): category, title and content are required", i, e.Title)
		}
		for _, k := range reservedSeedMetaKeys {
			if _, ok := e.Metadata[k]; ok {
				return nil, fmt.Errorf("platform knowledge base entry %d (%q): metadata key %q is reserved", i, e.Title, k)
			}
		}
		id := PlatformEntryID(e.Category, e.Title)
		if seen[id] {
			return nil, fmt.Errorf("platform knowledge base entry %d: duplicate category and title %q / %q", i, e.Category, e.Title)
		}
		seen[id] = true
	}
	return entries, nil
}

// PlatformEntryID is the derived id of a seeded platform entry. The key is the category and
// title, as the old seed matched on; a renamed entry is a new entry and the old one is retired.
func PlatformEntryID(category, title string) uuid.UUID {
	return uuid.NewSHA1(syncNamespace, []byte("platform_seed|"+category+"|"+title))
}

// legacyPlatformTitles are the category and title of every entry the old hard-coded
// seedCodevertexKB created (without a marker). They carried stale pricing and are superseded
// by platform_kb.json, so the sync retires them once. Kept as titles only, no content.
var legacyPlatformTitles = [][2]string{
	{"company", "Codevertex Africa Limited " + emDash + " Company Overview"},
	{"product", "Power Suite " + emDash + " ERP"},
	{"product", "Power Suite " + emDash + " POS (Point of Sale)"},
	{"product", "Power Suite " + emDash + " Books (Treasury / Accounting)"},
	{"product", "Power Suite " + emDash + " Analytics (Apache Superset)"},
	{"product", "Power Suite " + emDash + " MarketFlow CRM"},
	{"product", "Power Suite " + emDash + " ISP Billing"},
	{"product", "Power Suite " + emDash + " TruLoad (IoT Axle-Load Compliance)"},
	{"product", "Power Suite " + emDash + " Notifications Service"},
	{"education", "Digitika Academy " + emDash + " Overview"},
	{"education", "Digitika Academy " + emDash + " Software Engineering Courses"},
	{"education", "Digitika Academy " + emDash + " ICDL Courses"},
	{"education", "Digitika Academy " + emDash + " Networking & CCNA Courses"},
	{"education", "Digitika Academy " + emDash + " AI & Machine Learning Courses"},
	{"education", "Digitika Academy " + emDash + " Data Analytics Courses"},
}

// emDash is the dash the old seed used in its titles (U+2014).
var emDash = string(rune(0x2014))

// PlatformSyncResult counts what one SyncPlatformKB run did.
type PlatformSyncResult struct {
	Created   int
	Updated   int
	Unchanged int
	Retired   int
	// Skipped lists file entries left out because a hand-written platform entry (no marker)
	// already has the same category and title; that entry is never overwritten.
	Skipped []string
}

// SyncPlatformKB makes the platform knowledge base match entries. Rows live under tenantID, the
// platform-owner tenant (a storage home for the foreign key). raw is a direct connection; the
// embeddings table is not modelled by Ent.
func SyncPlatformKB(ctx context.Context, raw *sql.DB, tenantID uuid.UUID, entries []PlatformEntry) (PlatformSyncResult, error) {
	var res PlatformSyncResult
	if len(entries) == 0 {
		return res, errors.New("platform knowledge base is empty")
	}
	now := time.Now().UTC()

	// 1. The old seed's rows go first, so their titles never block a new entry.
	for _, lt := range legacyPlatformTitles {
		ids, err := queryIDs(ctx, raw, `
			SELECT id FROM knowledge_base_entries
			WHERE scope = 'platform' AND tenant_id = $1 AND category = $2 AND title = $3
			  AND metadata->>'`+metaSourceKind+`' IS NULL
			  AND COALESCE(metadata->>'`+metaStale+`', '') <> 'true'`, tenantID, lt[0], lt[1])
		if err != nil {
			return res, fmt.Errorf("find legacy platform entries: %w", err)
		}
		for _, id := range ids {
			out, err := retirePlatformEntry(ctx, raw, id, now)
			if err != nil {
				return res, err
			}
			if out == MadeStale {
				res.Retired++
			}
		}
	}

	// 2. Create or rewrite every file entry.
	want := make(map[uuid.UUID]bool, len(entries))
	for _, e := range entries {
		id := PlatformEntryID(e.Category, e.Title)
		want[id] = true
		out, err := upsertPlatformEntry(ctx, raw, tenantID, id, e, now)
		if err != nil {
			return res, fmt.Errorf("platform entry %q: %w", e.Title, err)
		}
		switch out {
		case Created:
			res.Created++
		case Updated:
			res.Updated++
		case Skipped:
			res.Skipped = append(res.Skipped, e.Category+" / "+e.Title)
		default:
			res.Unchanged++
		}
	}

	// 3. Retire seeded rows the file no longer has.
	ids, err := queryIDs(ctx, raw, `
		SELECT id FROM knowledge_base_entries
		WHERE scope = 'platform' AND tenant_id = $1
		  AND metadata->>'`+metaSourceKind+`' = '`+SourceKindPlatformSeed+`'
		  AND COALESCE(metadata->>'`+metaStale+`', '') <> 'true'`, tenantID)
	if err != nil {
		return res, fmt.Errorf("find seeded platform entries: %w", err)
	}
	for _, id := range ids {
		if want[id] {
			continue
		}
		out, err := retirePlatformEntry(ctx, raw, id, now)
		if err != nil {
			return res, err
		}
		if out == MadeStale {
			res.Retired++
		}
	}
	return res, nil
}

func queryIDs(ctx context.Context, raw *sql.DB, query string, args ...any) ([]uuid.UUID, error) {
	rows, err := raw.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// desiredMeta is the metadata a seeded entry should carry, without seeded_at (a timestamp is
// not a difference). It goes through JSON so it compares equal to what jsonb gives back.
func (e PlatformEntry) desiredMeta() (map[string]any, error) {
	out := make(map[string]any, len(e.Metadata)+2)
	for k, v := range e.Metadata {
		if k == metaSource {
			k = metaSeedSource
		}
		out[k] = v
	}
	out[metaSourceKind] = SourceKindPlatformSeed
	return normalizeJSON(out)
}

func normalizeJSON(m map[string]any) (map[string]any, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (e PlatformEntry) tags() []string {
	if e.Tags == nil {
		return []string{}
	}
	return e.Tags
}

// platformRow is the part of a stored entry the seed compares.
type platformRow struct {
	TenantID uuid.UUID
	Scope    string
	Category string
	Title    string
	Content  string
	Tags     []string
	Metadata map[string]any
}

// platformEntryChanged reports whether a stored row differs from what the file says. Any
// difference, including a stale flag or a missing marker, means a rewrite.
func platformEntryChanged(row platformRow, tenantID uuid.UUID, e PlatformEntry, want map[string]any) bool {
	if row.TenantID != tenantID || row.Scope != "platform" || row.Category != e.Category ||
		row.Title != e.Title || row.Content != e.Content {
		return true
	}
	if len(row.Tags) != len(e.Tags) {
		return true
	}
	for i := range row.Tags {
		if row.Tags[i] != e.Tags[i] {
			return true
		}
	}
	have := make(map[string]any, len(row.Metadata))
	for k, v := range row.Metadata {
		if k != metaSeededAt {
			have[k] = v
		}
	}
	return !reflect.DeepEqual(have, want)
}

// upsertPlatformEntry creates, rewrites or leaves one seeded entry.
func upsertPlatformEntry(ctx context.Context, raw *sql.DB, tenantID, id uuid.UUID, e PlatformEntry, now time.Time) (Outcome, error) {
	want, err := e.desiredMeta()
	if err != nil {
		return Skipped, err
	}
	stored := make(map[string]any, len(want)+1)
	for k, v := range want {
		stored[k] = v
	}
	stored[metaSeededAt] = now.Format(time.RFC3339)
	metaJSON, err := json.Marshal(stored)
	if err != nil {
		return Skipped, err
	}
	tagsJSON, err := json.Marshal(e.tags())
	if err != nil {
		return Skipped, err
	}

	tx, err := raw.BeginTx(ctx, nil)
	if err != nil {
		return Skipped, err
	}
	defer func() { _ = tx.Rollback() }()

	var (
		row              platformRow
		tagsRaw, metaRaw []byte
	)
	err = tx.QueryRowContext(ctx, `
		SELECT tenant_id, scope, category, title, content, tags, metadata
		FROM knowledge_base_entries WHERE id = $1 FOR UPDATE`, id).
		Scan(&row.TenantID, &row.Scope, &row.Category, &row.Title, &row.Content, &tagsRaw, &metaRaw)
	if errors.Is(err, sql.ErrNoRows) {
		// A hand-written platform entry with the same title is the owner's; never add a rival.
		var taken bool
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS (SELECT 1 FROM knowledge_base_entries
			WHERE scope = 'platform' AND category = $1 AND title = $2 AND id <> $3
			  AND metadata->>'`+metaSourceKind+`' IS NULL
			  AND COALESCE(metadata->>'`+metaStale+`', '') <> 'true')`, e.Category, e.Title, id).Scan(&taken); err != nil {
			return Skipped, err
		}
		if taken {
			return Skipped, nil
		}
		// Another pod inserting the same entry at the same moment loses harmlessly.
		r, err := tx.ExecContext(ctx, `
			INSERT INTO knowledge_base_entries
				(id, tenant_id, scope, category, title, content, embedded, chunk_count, tags, metadata, created_at, updated_at)
			VALUES ($1, $2, 'platform', $3, $4, $5, false, 0, $6::jsonb, $7::jsonb, $8, $8)
			ON CONFLICT (id) DO NOTHING`,
			id, tenantID, e.Category, e.Title, e.Content, string(tagsJSON), string(metaJSON), now)
		if err != nil {
			return Skipped, err
		}
		if err := tx.Commit(); err != nil {
			return Skipped, err
		}
		if n, _ := r.RowsAffected(); n == 0 {
			return Unchanged, nil
		}
		return Created, nil
	}
	if err != nil {
		return Skipped, err
	}
	if len(tagsRaw) > 0 {
		if err := json.Unmarshal(tagsRaw, &row.Tags); err != nil {
			return Skipped, fmt.Errorf("read tags: %w", err)
		}
	}
	if len(metaRaw) > 0 {
		if err := json.Unmarshal(metaRaw, &row.Metadata); err != nil {
			return Skipped, fmt.Errorf("read metadata: %w", err)
		}
	}
	if !platformEntryChanged(row, tenantID, e, want) {
		return Unchanged, nil
	}

	// Rewrite, drop the old vectors so a corrected fact (a price) stops answering at once, and
	// queue the entry for the embed sweep. Both in this transaction, under the row lock.
	if _, err := tx.ExecContext(ctx, `
		UPDATE knowledge_base_entries
		SET tenant_id = $2, scope = 'platform', category = $3, title = $4, content = $5,
		    tags = $6::jsonb, metadata = $7::jsonb, embedded = false, chunk_count = 0, updated_at = $8
		WHERE id = $1`,
		id, tenantID, e.Category, e.Title, e.Content, string(tagsJSON), string(metaJSON), now); err != nil {
		return Skipped, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM knowledge_base_embeddings WHERE entry_id = $1`, id); err != nil {
		return Skipped, fmt.Errorf("remove old embeddings: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Skipped, err
	}
	return Updated, nil
}

// retirePlatformEntry takes an entry out of search the Sprint 25 way (markStale in sync.go):
// metadata.stale, embeddings removed, embedded true with no chunks so the sweep leaves it. The
// row stays, so an entry put back in the file returns with its id.
func retirePlatformEntry(ctx context.Context, raw *sql.DB, id uuid.UUID, now time.Time) (Outcome, error) {
	tx, err := raw.BeginTx(ctx, nil)
	if err != nil {
		return Skipped, err
	}
	defer func() { _ = tx.Rollback() }()
	// The stale check sits in the WHERE clause: a second pod waits on the row lock, then finds
	// the row already stale and changes nothing.
	r, err := tx.ExecContext(ctx, `
		UPDATE knowledge_base_entries
		SET metadata = COALESCE(metadata, '{}'::jsonb) || jsonb_build_object('`+metaStale+`', true, '`+metaStaleAt+`', $2::text),
		    embedded = true, chunk_count = 0, updated_at = $3
		WHERE id = $1 AND scope = 'platform' AND COALESCE(metadata->>'`+metaStale+`', '') <> 'true'`,
		id, now.Format(time.RFC3339), now)
	if err != nil {
		return Skipped, fmt.Errorf("retire platform entry: %w", err)
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return Unchanged, nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM knowledge_base_embeddings WHERE entry_id = $1`, id); err != nil {
		return Skipped, fmt.Errorf("remove retired embeddings: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Skipped, err
	}
	return MadeStale, nil
}
