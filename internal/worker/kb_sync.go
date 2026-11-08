package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	sharedcache "github.com/Bengo-Hub/cache"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/knowledgebaseentry"
	"github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/bengobox/marketflow-api/internal/modules/creative"
	"github.com/bengobox/marketflow-api/internal/modules/knowledge"
	"github.com/bengobox/marketflow-api/internal/modules/marketingos"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"
)

// Knowledge base sync (Sprint 25): the worker side of modules/knowledge/sync.go. Inventory's item
// events keep names, descriptions and photos current within seconds; a nightly walk per tenant
// brings the real prices, catches deletions and new tenants; an embed sweep turns every changed
// entry into search vectors on the self-hosted Ollama.
//
// Only tenants that use MarketFlow are synced (a saved brand context or a team setup). Every POS
// and inventory tenant publishes these events, and embedding a catalogue nobody queries is wasted
// compute. A tenant that starts using MarketFlow is filled by the next nightly walk.

// usesMarketFlow reports whether a tenant's knowledge base is worth keeping in step.
func usesMarketFlow(meta map[string]any) bool {
	if _, ok := meta[marketingos.MetadataKey]; ok {
		return true
	}
	_, ok := meta[marketingos.TeamSetupKey]
	return ok
}

// KBItemHandler applies one inventory item event to the knowledge base.
type KBItemHandler struct {
	subject string
	durable string
	db      *ent.Client
	sync    *knowledge.Sync
	logger  *zap.Logger
}

// NewKBItemHandlers consumes both item events.
func NewKBItemHandlers(db *ent.Client, sync *knowledge.Sync, logger *zap.Logger) []Handler {
	l := logger.Named("kb-sync")
	return []Handler{
		&KBItemHandler{subject: "inventory.item.created", durable: "marketflow-kb-item-created", db: db, sync: sync, logger: l},
		&KBItemHandler{subject: "inventory.item.updated", durable: "marketflow-kb-item-updated", db: db, sync: sync, logger: l},
	}
}

func (h *KBItemHandler) Subject() string { return h.subject }
func (h *KBItemHandler) Durable() string { return h.durable }

type kbItemEvent struct {
	TenantID    string `json:"tenant_id"`
	ID          string `json:"id"`
	SKU         string `json:"sku"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Type        string `json:"type"`
	IsActive    bool   `json:"is_active"`
	// NotForSale is on item.created only, so an update cannot prove an item is for sale.
	NotForSale *bool  `json:"not_for_sale"`
	ImageURL   string `json:"image_url"`
	Controlled bool   `json:"is_controlled_substance"`
}

func (e kbItemEvent) sourceItem(tid uuid.UUID) knowledge.SourceItem {
	notForSale := e.NotForSale != nil && *e.NotForSale
	return knowledge.SourceItem{TenantID: tid, Source: knowledge.SourceInventory, SourceID: e.ID, Kind: e.Type,
		Name: e.Name, Description: e.Description, SKU: e.SKU, ImageURL: httpURL(e.ImageURL),
		Sellable:   e.IsActive && !notForSale && !e.Controlled && creative.IsSellableType(e.Type),
		UpdateOnly: e.NotForSale == nil}
}

func httpURL(u string) string {
	if strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://") {
		return u
	}
	return ""
}

func (h *KBItemHandler) Handle(ctx context.Context, msg jetstream.Msg) error {
	var ev kbItemEvent
	if err := json.Unmarshal(eventBody(msg.Data()), &ev); err != nil {
		return dropPoison(h.logger, msg, "decode "+h.subject, err)
	}
	tid, err := uuid.Parse(ev.TenantID)
	if err != nil || ev.ID == "" {
		return dropPoison(h.logger, msg, h.subject+" without tenant or item", fmt.Errorf("tenant %q", ev.TenantID))
	}
	return h.process(ctx, tid, ev)
}

func (h *KBItemHandler) process(ctx context.Context, tid uuid.UUID, ev kbItemEvent) error {
	t, err := h.db.Tenant.Get(ctx, tid)
	if ent.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !usesMarketFlow(t.Metadata) {
		return nil
	}
	_, err = h.sync.Apply(ctx, ev.sourceItem(tid))
	return err
}

// Embed sweep limits: Ollama is shared, so a bulk import is worked through at a gentle rate.
const (
	kbEmbedEvery    = time.Minute
	kbEmbedBatch    = 20
	kbEmbedFailures = 3
)

// KBEmbedSweep embeds entries waiting for it. Nothing publishes embedding.requested, so before
// this an entry saved in the portal reached search only through a manual backfill.
type KBEmbedSweep struct {
	db     *ent.Client
	embed  *EmbeddingHandler
	logger *zap.Logger
	claim  func(ctx context.Context, name string, period time.Duration) bool
}

func NewKBEmbedSweep(db *ent.Client, embed *EmbeddingHandler, logger *zap.Logger) *KBEmbedSweep {
	return &KBEmbedSweep{db: db, embed: embed, logger: logger.Named("kb-embed"), claim: sharedcache.ClaimPeriod}
}

// Run ticks each minute; the claim keeps it to one pod per minute.
func (s *KBEmbedSweep) Run(ctx context.Context) {
	t := time.NewTicker(kbEmbedEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if s.claim(ctx, "marketflow-kb-embed", kbEmbedEvery) {
				s.tick(ctx)
			}
		}
	}
}

func (s *KBEmbedSweep) tick(ctx context.Context) {
	tctx, cancel := context.WithTimeout(ctx, kbEmbedEvery-5*time.Second)
	defer cancel()
	rows, err := s.db.KnowledgeBaseEntry.Query().
		Where(knowledgebaseentry.Embedded(false)).
		Order(ent.Asc(knowledgebaseentry.FieldUpdatedAt)).
		Limit(kbEmbedBatch).All(tctx)
	if err != nil {
		s.logger.Warn("load entries to embed", zap.Error(err))
		return
	}
	failures := 0
	for _, e := range rows {
		if err := s.embed.embedEntry(tctx, e); err != nil {
			// One bad entry must not hold the queue; several in a row means Ollama is down.
			s.logger.Warn("embed entry", zap.String("entry", e.ID.String()), zap.Error(err))
			if failures++; failures >= kbEmbedFailures {
				return
			}
			continue
		}
		failures = 0
	}
}

// Nightly walk limits.
const (
	kbWalkPage   = 100
	kbTenantPage = 200
)

// KBSourceSweep walks each MarketFlow tenant's catalogue once a day.
type KBSourceSweep struct {
	db        *ent.Client
	sync      *knowledge.Sync
	offerings *creative.Offerings
	logger    *zap.Logger
	claim     func(ctx context.Context, key string, ttl time.Duration) bool
	now       func() time.Time
}

func NewKBSourceSweep(db *ent.Client, sync *knowledge.Sync, offerings *creative.Offerings, logger *zap.Logger) *KBSourceSweep {
	return &KBSourceSweep{db: db, sync: sync, offerings: offerings, logger: logger.Named("kb-walk"), claim: sharedcache.ClaimOnce, now: time.Now}
}

// Run checks hourly; each tenant is walked once per UTC day (the claim key).
func (s *KBSourceSweep) Run(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		s.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *KBSourceSweep) tick(ctx context.Context) {
	day := s.now().UTC().Format("2006-01-02")
	var after uuid.UUID
	for ctx.Err() == nil {
		q := s.db.Tenant.Query().
			Where(func(sel *entsql.Selector) {
				m := sel.C(tenant.FieldMetadata)
				sel.Where(entsql.ExprP(m + "->'" + marketingos.MetadataKey + "' IS NOT NULL OR " + m + "->'" + marketingos.TeamSetupKey + "' IS NOT NULL"))
			}).
			Order(ent.Asc(tenant.FieldID)).Limit(kbTenantPage)
		if after != uuid.Nil {
			q = q.Where(tenant.IDGT(after))
		}
		rows, err := q.All(ctx)
		if err != nil {
			s.logger.Warn("load tenants", zap.Error(err))
			return
		}
		for _, t := range rows {
			if s.claim(ctx, fmt.Sprintf("marketflow:kb-walk:%s:%s", t.ID, day), 36*time.Hour) {
				s.walk(ctx, t.ID)
			}
		}
		if len(rows) < kbTenantPage {
			return
		}
		after = rows[len(rows)-1].ID
	}
}

// walk applies the tenant's whole catalogue. Stale marking needs a walk that finished: a page that
// failed proves nothing about the items it did not read.
func (s *KBSourceSweep) walk(ctx context.Context, tid uuid.UUID) {
	seen := map[string]bool{}
	complete := false
	for offset := 0; offset < knowledge.MaxSyncedEntries*2; offset += kbWalkPage {
		page, raw, err := s.offerings.CataloguePage(ctx, tid, offset, kbWalkPage)
		if err != nil {
			s.logger.Info("catalogue unavailable", zap.String("tenant", tid.String()), zap.Error(err))
			break
		}
		for _, off := range page {
			seen[off.SourceID] = true
			if err := s.apply(ctx, tid, off, knowledge.SourceInventory); err != nil {
				s.logger.Warn("apply item", zap.String("tenant", tid.String()), zap.Error(err))
				return
			}
		}
		if raw < kbWalkPage {
			complete = true
			break
		}
	}
	if complete {
		if _, err := s.sync.MarkMissing(ctx, tid, knowledge.SourceInventory, seen); err != nil {
			s.logger.Warn("mark missing items", zap.String("tenant", tid.String()), zap.Error(err))
		}
	}
	// The website catalogue exists for the platform tenant only. It comes from two endpoints and
	// may arrive with one missing, so stale marking needs both courses and products in hand.
	list, ok := s.offerings.ExtraOfferings(ctx, tid)
	if !ok {
		return
	}
	webSeen := map[string]bool{}
	courses, others := 0, 0
	for _, off := range list {
		webSeen[off.SourceID] = true
		if off.Type == "COURSE" {
			courses++
		} else {
			others++
		}
		if err := s.apply(ctx, tid, off, knowledge.SourceWebsite); err != nil {
			s.logger.Warn("apply website item", zap.String("tenant", tid.String()), zap.Error(err))
			return
		}
	}
	if courses == 0 || others == 0 {
		return
	}
	if _, err := s.sync.MarkMissing(ctx, tid, knowledge.SourceWebsite, webSeen); err != nil {
		s.logger.Warn("mark missing website items", zap.String("tenant", tid.String()), zap.Error(err))
	}
}

func (s *KBSourceSweep) apply(ctx context.Context, tid uuid.UUID, off creative.Offering, source string) error {
	it := knowledge.SourceItem{TenantID: tid, Source: source, SourceID: off.SourceID, Kind: off.Type, Name: off.Name,
		Description: off.Description, SKU: off.SKU, Price: off.Price, Sellable: true}
	if len(off.PhotoURLs) > 0 {
		it.ImageURL = off.PhotoURLs[0]
	}
	_, err := s.sync.Apply(ctx, it)
	return err
}
