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

// Seeded knowledge base: the entries Vera answers from that ship with the binary, in seed sets
// (seed_sets.go). The first set is the Codevertex platform knowledge base (company, Power Suite
// products, pricing, Digitika Academy); the others are the Kenya tax guide and the FLP product,
// FAQ, business opportunity and compliance sets.
//
// Each set has one source, a JSON file built into the binary. The seed binary (cmd/seed, run by
// scripts/entrypoint.sh on every API pod start) calls SyncSeedSet for each set, which makes the
// database match the file:
//   - an entry missing from the database is created;
//   - an entry whose words, tags, product fields or metadata differ is rewritten, its search
//     vectors are deleted and it is queued for embedding (embedded = false), so the worker's
//     embed sweep (worker/kb_sync.go) re-embeds it within a minute. A portal edit of a seeded
//     entry counts as a difference too: the file wins, so edit the file instead;
//   - an entry the set created that is no longer in its file is retired the Sprint 25 way
//     (metadata.stale, embeddings removed), never deleted.
//
// A seeded entry's id is derived from its set's marker, category and title (plus the tenant for a
// tenant-scope run), and its metadata carries the set's marker as source_kind. A set only ever
// retires rows with its own marker in the scope and tenant it runs for, plus the rows the old
// create-only seed made (each set's Legacy list); other sets' rows, tenant rows and hand-written
// rows are never touched.
//
// Several pods may run the seed at once. Creates are insert-or-ignore on the derived id, and
// every rewrite or retirement locks its row in a short transaction and re-checks it, so a second
// pod finds nothing left to do. Nothing here needs a session advisory lock, which PgBouncer
// cannot hold.

//go:embed platform_kb.json
var platformKBFile []byte

// Scopes a seed run writes in. The seed binary takes it from SEED_SCOPE.
const (
	ScopePlatform = "platform"
	ScopeTenant   = "tenant"
)

// Metadata keys written by the seed.
const (
	metaSourceKind = "source_kind"
	metaSeededAt   = "seeded_at"
	// metaSeedSource holds the file's own "source" field (where a fact was checked). The key
	// "source" means an entry synced from a business's system (sync.go), so it is renamed.
	metaSeedSource = "seed_source"

	// SourceKindPlatformSeed marks an entry created and owned by the Codevertex platform set.
	SourceKindPlatformSeed = "platform_seed"
)

// reservedSeedMetaKeys may not appear in a file entry's metadata: the seed writes them itself.
var reservedSeedMetaKeys = []string{metaSourceKind, metaSeededAt, metaSeedSource, metaStale, metaStaleAt}

// PlatformEntry is one entry of a seed set's file. Scope is only given in platform_kb.json
// (always platform); the other sets take the scope of the seed run, as the old seed did.
type PlatformEntry struct {
	Scope       string         `json:"scope,omitempty"`
	Category    string         `json:"category"`
	Title       string         `json:"title"`
	Content     string         `json:"content"`
	Tags        []string       `json:"tags,omitempty"`
	ProductCode *string        `json:"product_code,omitempty"`
	ProductCC   *float64       `json:"product_cc,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// LegacySeedRow names a row the old create-only seed made, without a marker. A set retires the
// rows it lists once, before its own entries are written, so their titles never block them. A
// row is matched on category and title, or on category and product code when ProductCode is set
// (the old product seed matched on the code, so a product renamed since keeps its old title).
type LegacySeedRow struct {
	Category    string
	Title       string
	ProductCode string
}

// SeedSet is one seeded part of the knowledge base.
type SeedSet struct {
	// Name is used in logs and errors.
	Name string
	// SourceKind is the metadata.source_kind marker of the set's rows. It must never change, and
	// no two sets may share one.
	SourceKind string
	// PlatformOnly sets are written only by a platform-scope run.
	PlatformOnly bool
	// Entries reads the set's file.
	Entries func() ([]PlatformEntry, error)
	// Legacy lists the rows the old create-only seed made for this set's content.
	Legacy []LegacySeedRow
}

// PlatformEntries returns the Codevertex platform knowledge base shipped in this binary.
func PlatformEntries() ([]PlatformEntry, error) {
	return parsePlatformKB(platformKBFile)
}

func parsePlatformKB(data []byte) ([]PlatformEntry, error) {
	return parseSeedEntries("platform knowledge base", data, ScopePlatform)
}

// parseSeedEntries reads a set file: a JSON array of entries. fixedScope is the scope every
// entry must name, or empty when the file leaves the scope to the seed run.
func parseSeedEntries(label string, data []byte, fixedScope string) ([]PlatformEntry, error) {
	var entries []PlatformEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parse %s: %w", label, err)
	}
	return validateSeedEntries(label, entries, fixedScope)
}

func validateSeedEntries(label string, entries []PlatformEntry, fixedScope string) ([]PlatformEntry, error) {
	if len(entries) == 0 {
		// An empty file would retire every seeded entry of the set, so it is refused.
		return nil, fmt.Errorf("%s is empty", label)
	}
	seen := make(map[string]bool, len(entries))
	for i, e := range entries {
		if fixedScope != "" && e.Scope != fixedScope {
			return nil, fmt.Errorf("%s entry %d (%q): scope must be %s, got %q", label, i, e.Title, fixedScope, e.Scope)
		}
		if fixedScope == "" && e.Scope != "" {
			return nil, fmt.Errorf("%s entry %d (%q): scope comes from the seed run, leave it out", label, i, e.Title)
		}
		if strings.TrimSpace(e.Category) == "" || strings.TrimSpace(e.Title) == "" || strings.TrimSpace(e.Content) == "" {
			return nil, fmt.Errorf("%s entry %d (%q): category, title and content are required", label, i, e.Title)
		}
		for _, k := range reservedSeedMetaKeys {
			if _, ok := e.Metadata[k]; ok {
				return nil, fmt.Errorf("%s entry %d (%q): metadata key %q is reserved", label, i, e.Title, k)
			}
		}
		key := e.Category + "|" + e.Title
		if seen[key] {
			return nil, fmt.Errorf("%s entry %d: duplicate category and title %q / %q", label, i, e.Category, e.Title)
		}
		seen[key] = true
	}
	return entries, nil
}

// PlatformEntryID is the derived id of a Codevertex platform entry. The key is the category and
// title, as the old seed matched on; a renamed entry is a new entry and the old one is retired.
func PlatformEntryID(category, title string) uuid.UUID {
	return SeedEntryID(SourceKindPlatformSeed, ScopePlatform, uuid.Nil, category, title)
}

// SeedEntryID is the derived id of a seeded entry. A platform entry exists once whatever tenant
// anchors it, so its id leaves the tenant out (for the platform_seed set this is the id the
// Codevertex sync has always used). A tenant-scope entry exists once per tenant.
func SeedEntryID(sourceKind, scope string, tenantID uuid.UUID, category, title string) uuid.UUID {
	key := sourceKind + "|" + category + "|" + title
	if scope != ScopePlatform {
		key = scope + "|" + tenantID.String() + "|" + key
	}
	return uuid.NewSHA1(syncNamespace, []byte(key))
}

// codevertexLegacy names every entry the old hard-coded seedCodevertexKB created (without a
// marker). They carried stale pricing and are superseded by platform_kb.json, so the sync
// retires them once. Kept as titles only, no content.
var codevertexLegacy = []LegacySeedRow{
	{Category: "company", Title: "Codevertex Africa Limited " + emDash + " Company Overview"},
	{Category: "product", Title: "Power Suite " + emDash + " ERP"},
	{Category: "product", Title: "Power Suite " + emDash + " POS (Point of Sale)"},
	{Category: "product", Title: "Power Suite " + emDash + " Books (Treasury / Accounting)"},
	{Category: "product", Title: "Power Suite " + emDash + " Analytics (Apache Superset)"},
	{Category: "product", Title: "Power Suite " + emDash + " MarketFlow CRM"},
	{Category: "product", Title: "Power Suite " + emDash + " ISP Billing"},
	{Category: "product", Title: "Power Suite " + emDash + " TruLoad (IoT Axle-Load Compliance)"},
	{Category: "product", Title: "Power Suite " + emDash + " Notifications Service"},
	{Category: "education", Title: "Digitika Academy " + emDash + " Overview"},
	{Category: "education", Title: "Digitika Academy " + emDash + " Software Engineering Courses"},
	{Category: "education", Title: "Digitika Academy " + emDash + " ICDL Courses"},
	{Category: "education", Title: "Digitika Academy " + emDash + " Networking & CCNA Courses"},
	{Category: "education", Title: "Digitika Academy " + emDash + " AI & Machine Learning Courses"},
	{Category: "education", Title: "Digitika Academy " + emDash + " Data Analytics Courses"},
}

// emDash is the dash the old seed used in its titles (U+2014).
var emDash = string(rune(0x2014))

// CodevertexSeedSet is the Codevertex platform knowledge base.
func CodevertexSeedSet() SeedSet {
	return SeedSet{
		Name:         "codevertex",
		SourceKind:   SourceKindPlatformSeed,
		PlatformOnly: true,
		Entries:      PlatformEntries,
		Legacy:       codevertexLegacy,
	}
}

// PlatformSyncResult counts what one sync of a seed set did.
type PlatformSyncResult struct {
	Created   int
	Updated   int
	Unchanged int
	Retired   int
	// Skipped lists file entries left out because a hand-written entry (no marker) in the same
	// scope already has the same category and title; that entry is never overwritten.
	Skipped []string
}

// SyncPlatformKB makes the Codevertex platform knowledge base match entries. Rows live under
// tenantID, the platform-owner tenant (a storage home for the foreign key).
func SyncPlatformKB(ctx context.Context, raw *sql.DB, tenantID uuid.UUID, entries []PlatformEntry) (PlatformSyncResult, error) {
	return SyncSeedSet(ctx, raw, CodevertexSeedSet(), ScopePlatform, tenantID, entries)
}

// SyncSeedSet makes one seed set's rows in scope, anchored to tenantID, match entries. A
// platform-scope run anchors rows to the platform-owner tenant; a tenant-scope run writes the
// tenant's own copy, as the old seed did with SEED_SCOPE=tenant. raw is a direct connection; the
// embeddings table is not modelled by Ent.
func SyncSeedSet(ctx context.Context, raw *sql.DB, set SeedSet, scope string, tenantID uuid.UUID, entries []PlatformEntry) (PlatformSyncResult, error) {
	var res PlatformSyncResult
	switch {
	case strings.TrimSpace(set.SourceKind) == "":
		return res, fmt.Errorf("seed set %q has no source kind", set.Name)
	case scope != ScopePlatform && scope != ScopeTenant:
		return res, fmt.Errorf("seed set %q: unknown scope %q", set.Name, scope)
	case set.PlatformOnly && scope != ScopePlatform:
		return res, fmt.Errorf("seed set %q is platform only", set.Name)
	case tenantID == uuid.Nil:
		return res, fmt.Errorf("seed set %q: no tenant to anchor rows to", set.Name)
	case len(entries) == 0:
		return res, fmt.Errorf("seed set %q is empty", set.Name)
	}
	for _, e := range entries {
		if e.Scope != "" && e.Scope != scope {
			return res, fmt.Errorf("seed set %q entry %q: scope %q in a %s run", set.Name, e.Title, e.Scope, scope)
		}
	}
	now := time.Now().UTC()

	// 1. The old seed's rows go first, so their titles never block a new entry. Rows synced from a
	// business's system (metadata.source) are never the old seed's.
	for _, lr := range set.Legacy {
		match, key := "title", lr.Title
		if lr.ProductCode != "" {
			match, key = "product_code", lr.ProductCode
		}
		ids, err := queryIDs(ctx, raw, `
			SELECT id FROM knowledge_base_entries
			WHERE scope = $1 AND tenant_id = $2 AND category = $3 AND `+match+` = $4
			  AND metadata->>'`+metaSourceKind+`' IS NULL
			  AND metadata->>'`+metaSource+`' IS NULL
			  AND COALESCE(metadata->>'`+metaStale+`', '') <> 'true'`, scope, tenantID, lr.Category, key)
		if err != nil {
			return res, fmt.Errorf("find legacy %s entries: %w", set.Name, err)
		}
		for _, id := range ids {
			out, err := retireSeedEntry(ctx, raw, id, scope, now)
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
		id := SeedEntryID(set.SourceKind, scope, tenantID, e.Category, e.Title)
		want[id] = true
		out, err := upsertSeedEntry(ctx, raw, set.SourceKind, scope, tenantID, id, e, now)
		if err != nil {
			return res, fmt.Errorf("%s entry %q: %w", set.Name, e.Title, err)
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

	// 3. Retire this set's rows the file no longer has, in this scope and tenant only.
	ids, err := queryIDs(ctx, raw, `
		SELECT id FROM knowledge_base_entries
		WHERE scope = $1 AND tenant_id = $2
		  AND metadata->>'`+metaSourceKind+`' = $3
		  AND COALESCE(metadata->>'`+metaStale+`', '') <> 'true'`, scope, tenantID, set.SourceKind)
	if err != nil {
		return res, fmt.Errorf("find seeded %s entries: %w", set.Name, err)
	}
	for _, id := range ids {
		if want[id] {
			continue
		}
		out, err := retireSeedEntry(ctx, raw, id, scope, now)
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
func (e PlatformEntry) desiredMeta(sourceKind string) (map[string]any, error) {
	out := make(map[string]any, len(e.Metadata)+2)
	for k, v := range e.Metadata {
		if k == metaSource {
			k = metaSeedSource
		}
		out[k] = v
	}
	out[metaSourceKind] = sourceKind
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
	TenantID    uuid.UUID
	Scope       string
	Category    string
	Title       string
	Content     string
	Tags        []string
	ProductCode *string
	ProductCC   *float64
	Metadata    map[string]any
}

// platformEntryChanged reports whether a stored row differs from what the file says. Any
// difference, including a stale flag or a missing marker, means a rewrite.
func platformEntryChanged(row platformRow, scope string, tenantID uuid.UUID, e PlatformEntry, want map[string]any) bool {
	if row.TenantID != tenantID || row.Scope != scope || row.Category != e.Category ||
		row.Title != e.Title || row.Content != e.Content {
		return true
	}
	if !sameString(row.ProductCode, e.ProductCode) || !sameFloat(row.ProductCC, e.ProductCC) {
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

func sameString(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameFloat(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// upsertSeedEntry creates, rewrites or leaves one seeded entry.
func upsertSeedEntry(ctx context.Context, raw *sql.DB, sourceKind, scope string, tenantID, id uuid.UUID, e PlatformEntry, now time.Time) (Outcome, error) {
	want, err := e.desiredMeta(sourceKind)
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
		code             sql.NullString
		cc               sql.NullFloat64
	)
	err = tx.QueryRowContext(ctx, `
		SELECT tenant_id, scope, category, title, content, tags, product_code, product_cc, metadata
		FROM knowledge_base_entries WHERE id = $1 FOR UPDATE`, id).
		Scan(&row.TenantID, &row.Scope, &row.Category, &row.Title, &row.Content, &tagsRaw, &code, &cc, &metaRaw)
	if errors.Is(err, sql.ErrNoRows) {
		// A hand-written entry with the same title in the same scope (and, for a tenant run, the
		// same tenant) is the owner's; never add a rival.
		q := `
			SELECT EXISTS (SELECT 1 FROM knowledge_base_entries
			WHERE scope = $1 AND category = $2 AND title = $3 AND id <> $4
			  AND metadata->>'` + metaSourceKind + `' IS NULL
			  AND COALESCE(metadata->>'` + metaStale + `', '') <> 'true'`
		args := []any{scope, e.Category, e.Title, id}
		if scope != ScopePlatform {
			q += ` AND tenant_id = $5`
			args = append(args, tenantID)
		}
		var taken bool
		if err := tx.QueryRowContext(ctx, q+`)`, args...).Scan(&taken); err != nil {
			return Skipped, err
		}
		if taken {
			return Skipped, nil
		}
		// Another pod inserting the same entry at the same moment loses harmlessly.
		r, err := tx.ExecContext(ctx, `
			INSERT INTO knowledge_base_entries
				(id, tenant_id, scope, category, title, content, product_code, product_cc,
				 embedded, chunk_count, tags, metadata, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, false, 0, $9::jsonb, $10::jsonb, $11, $11)
			ON CONFLICT (id) DO NOTHING`,
			id, tenantID, scope, e.Category, e.Title, e.Content, e.ProductCode, e.ProductCC,
			string(tagsJSON), string(metaJSON), now)
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
	if code.Valid {
		row.ProductCode = &code.String
	}
	if cc.Valid {
		row.ProductCC = &cc.Float64
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
	if !platformEntryChanged(row, scope, tenantID, e, want) {
		return Unchanged, nil
	}

	// Rewrite, drop the old vectors so a corrected fact (a price) stops answering at once, and
	// queue the entry for the embed sweep. Both in this transaction, under the row lock.
	if _, err := tx.ExecContext(ctx, `
		UPDATE knowledge_base_entries
		SET tenant_id = $2, scope = $3, category = $4, title = $5, content = $6,
		    product_code = $7, product_cc = $8,
		    tags = $9::jsonb, metadata = $10::jsonb, embedded = false, chunk_count = 0, updated_at = $11
		WHERE id = $1`,
		id, tenantID, scope, e.Category, e.Title, e.Content, e.ProductCode, e.ProductCC,
		string(tagsJSON), string(metaJSON), now); err != nil {
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

// retireSeedEntry takes an entry out of search the Sprint 25 way (markStale in sync.go):
// metadata.stale, embeddings removed, embedded true with no chunks so the sweep leaves it. The
// row stays, so an entry put back in the file returns with its id.
func retireSeedEntry(ctx context.Context, raw *sql.DB, id uuid.UUID, scope string, now time.Time) (Outcome, error) {
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
		WHERE id = $1 AND scope = $4 AND COALESCE(metadata->>'`+metaStale+`', '') <> 'true'`,
		id, now.Format(time.RFC3339), now, scope)
	if err != nil {
		return Skipped, fmt.Errorf("retire seeded entry: %w", err)
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
