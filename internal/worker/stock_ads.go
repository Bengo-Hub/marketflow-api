package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/campaign"
	"github.com/bengobox/marketflow-api/internal/ent/creativebrief"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"
)

// Stock-driven boosts (Sprint 20): an ad must not keep selling what the business cannot deliver.
// inventory.stock.out pauses the running boosts of that item, and a later stock movement that
// leaves stock resumes only the boosts stock paused, never one the owner paused by hand.
//
// affects_availability is the tenant's auto-hide setting. When it is false the item keeps
// selling into negative stock and the event is only an alert, so the ads keep running too.

const stockPausedKey = "stock_paused_item"

// StockOutHandler pauses boosts when an item runs out.
type StockOutHandler struct {
	db     *ent.Client
	ops    *BoostOps
	logger *zap.Logger
}

// StockBackHandler resumes them when it is back.
type StockBackHandler struct {
	db     *ent.Client
	ops    *BoostOps
	logger *zap.Logger
	now    func() time.Time
}

func NewStockAdsHandlers(db *ent.Client, ops *BoostOps, logger *zap.Logger) []Handler {
	l := logger.Named("stock-ads")
	return []Handler{&StockOutHandler{db: db, ops: ops, logger: l}, &StockBackHandler{db: db, ops: ops, logger: l, now: time.Now}}
}

func (*StockOutHandler) Subject() string  { return "inventory.stock.out" }
func (*StockOutHandler) Durable() string  { return "marketflow-stock-out-ads" }
func (*StockBackHandler) Subject() string { return "inventory.stock.updated" }
func (*StockBackHandler) Durable() string { return "marketflow-stock-back-ads" }

type stockEvent struct {
	TenantID            string  `json:"tenant_id"`
	ItemID              string  `json:"item_id"`
	Name                string  `json:"name"`
	AffectsAvailability bool    `json:"affects_availability"`
	QuantityAfter       float64 `json:"quantity_after"`
}

func decodeStock(data []byte) (uuid.UUID, stockEvent, error) {
	var ev stockEvent
	if err := json.Unmarshal(eventBody(data), &ev); err != nil {
		return uuid.Nil, ev, err
	}
	tid, err := uuid.Parse(ev.TenantID)
	if err != nil || ev.ItemID == "" {
		return uuid.Nil, ev, fmt.Errorf("stock event without tenant or item: %v", err)
	}
	return tid, ev, nil
}

func (h *StockOutHandler) Handle(ctx context.Context, msg jetstream.Msg) error {
	tid, ev, err := decodeStock(msg.Data())
	if err != nil {
		return dropPoison(h.logger, msg, "decode inventory.stock.out", err)
	}
	if !ev.AffectsAvailability {
		return nil
	}
	versions, err := h.versionsForItem(ctx, tid, ev.ItemID)
	if err != nil || len(versions) == 0 {
		return err
	}
	rows, err := h.db.Campaign.Query().
		Where(campaign.TenantIDEQ(tid), campaign.StatusEQ("active"), campaign.CreativeVersionIDIn(versions...)).
		All(ctx)
	if err != nil {
		return err
	}
	for _, c := range rows {
		reason := "the item in this ad is out of stock"
		if ev.Name != "" {
			reason = ev.Name + " is out of stock"
		}
		if err := h.ops.PauseForStock(ctx, c, ev.ItemID, reason); err != nil {
			return fmt.Errorf("pause boost %s: %w", c.ID, err) // retried: Meta may be briefly down
		}
	}
	return nil
}

// versionsForItem finds the creative versions advertising an inventory item, through the briefs
// whose offering is that item. The tenant filter keeps the read small.
func (h *StockOutHandler) versionsForItem(ctx context.Context, tid uuid.UUID, itemID string) ([]uuid.UUID, error) {
	briefs, err := h.db.CreativeBrief.Query().
		Where(creativebrief.TenantID(tid), func(s *entsql.Selector) {
			s.Where(entsql.P(func(b *entsql.Builder) {
				b.WriteString(s.C(creativebrief.FieldOffering) + "->>'source_id' = ").Arg(itemID)
			}))
		}).IDs(ctx)
	if err != nil || len(briefs) == 0 {
		return nil, err
	}
	return h.db.CreativeVersion.Query().Where(creativeversion.BriefIDIn(briefs...)).IDs(ctx)
}

func (h *StockBackHandler) Handle(ctx context.Context, msg jetstream.Msg) error {
	tid, ev, err := decodeStock(msg.Data())
	if err != nil {
		return dropPoison(h.logger, msg, "decode inventory.stock.updated", err)
	}
	if ev.QuantityAfter <= 0 {
		return nil
	}
	// Stock moves all day; this read is what most events cost: paused boosts of one tenant.
	rows, err := h.db.Campaign.Query().
		Where(campaign.TenantIDEQ(tid), campaign.StatusEQ("paused"), func(s *entsql.Selector) {
			s.Where(entsql.P(func(b *entsql.Builder) {
				b.WriteString(s.C(campaign.FieldMetadata) + "->>'" + stockPausedKey + "' = ").Arg(ev.ItemID)
			}))
		}).All(ctx)
	if err != nil {
		return err
	}
	for _, c := range rows {
		if c.EndDate != nil && !c.EndDate.After(h.now()) {
			continue // the boost's period is over; it stays paused
		}
		if err := h.ops.ResumeAfterStock(ctx, c); err != nil {
			return fmt.Errorf("resume boost %s: %w", c.ID, err)
		}
	}
	return nil
}

// PauseForStock pauses a boost on Meta and marks which item paused it.
func (b *BoostOps) PauseForStock(ctx context.Context, c *ent.Campaign, itemID, reason string) error {
	tok, ok := b.AdToken(ctx, c)
	if !ok || c.MetaCampaignID == nil {
		return nil // nothing we can pause on Meta: not a boost on a connected account
	}
	if err := b.meta.SetStatus(ctx, tok, *c.MetaCampaignID, "PAUSED"); err != nil {
		return err
	}
	if err := b.setStatusMeta(ctx, c, "active", "paused", map[string]any{stockPausedKey: itemID}); err != nil {
		return err
	}
	b.tell(ctx, c, reason)
	return nil
}

// ResumeAfterStock sets a stock-paused boost running again and clears the mark.
func (b *BoostOps) ResumeAfterStock(ctx context.Context, c *ent.Campaign) error {
	tok, ok := b.AdToken(ctx, c)
	if !ok || c.MetaCampaignID == nil {
		return nil
	}
	if err := b.meta.SetStatus(ctx, tok, *c.MetaCampaignID, "ACTIVE"); err != nil {
		return err
	}
	return b.setStatusMeta(ctx, c, "paused", "active", map[string]any{stockPausedKey: nil})
}

// setStatusMeta moves the campaign's status and merges metadata in one statement, only from the
// expected status; a nil value removes the key's meaning (jsonb null reads as empty).
func (b *BoostOps) setStatusMeta(ctx context.Context, c *ent.Campaign, from, to string, patch map[string]any) error {
	raw, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	value := entsql.ExprFunc(func(bd *entsql.Builder) {
		bd.WriteString("(COALESCE(" + campaign.FieldMetadata + ", '{}'::jsonb) || ")
		bd.Arg(string(raw))
		bd.WriteString("::jsonb)")
	})
	_, err = b.db.Campaign.Update().
		Where(campaign.IDEQ(c.ID), campaign.StatusEQ(from)).
		SetStatus(to).
		Modify(func(u *entsql.UpdateBuilder) { u.Set(campaign.FieldMetadata, value) }).
		Save(ctx)
	if err != nil {
		b.logger.Warn("boost status not saved", zap.String("campaign", c.ID.String()), zap.Error(err))
	}
	return err
}
