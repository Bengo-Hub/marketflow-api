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
	"github.com/bengobox/marketflow-api/internal/ent/agentrun"
	"github.com/bengobox/marketflow-api/internal/ent/customagent"
	"github.com/bengobox/marketflow-api/internal/modules/creative"
	"github.com/bengobox/marketflow-api/internal/modules/custom_agents"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"
)

// triggeredByNewItem marks a run started by a new catalogue item.
const triggeredByNewItem = "rule:new_item"

// NewItemAdsHandler drafts a launch ad when a new item joins the catalogue (Sprint 20). Each draft
// costs credits, so an agent must opt in, and it makes at most one a day: a bulk import of a
// thousand items costs one draft, and the rest are not queued. The draft waits for the owner's
// review like any other.
type NewItemAdsHandler struct {
	db     *ent.Client
	runner *AutopilotRunner
	logger *zap.Logger
	now    func() time.Time
	claim  func(ctx context.Context, key string, ttl time.Duration) bool
}

func NewNewItemAdsHandler(db *ent.Client, runner *AutopilotRunner, logger *zap.Logger) *NewItemAdsHandler {
	return &NewItemAdsHandler{db: db, runner: runner, logger: logger.Named("new-item-ads"), now: time.Now, claim: sharedcache.ClaimOnce}
}

func (*NewItemAdsHandler) Subject() string { return "inventory.item.created" }
func (*NewItemAdsHandler) Durable() string { return "marketflow-new-item-ads" }

type itemCreatedEvent struct {
	TenantID    string `json:"tenant_id"`
	ID          string `json:"id"`
	SKU         string `json:"sku"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Type        string `json:"type"`
	IsActive    bool   `json:"is_active"`
	NotForSale  bool   `json:"not_for_sale"`
	ImageURL    string `json:"image_url"`
	AgeLimited  bool   `json:"requires_age_verification"`
	Controlled  bool   `json:"is_controlled_substance"`
}

// advertisable keeps the draft to items an ad can honestly show: active, for sale, with a real
// photo, and outside the categories that need an owner's own judgement.
func (e itemCreatedEvent) advertisable() bool {
	return e.IsActive && !e.NotForSale && !e.AgeLimited && !e.Controlled && creative.IsSellableType(e.Type) &&
		strings.TrimSpace(e.Name) != "" && (strings.HasPrefix(e.ImageURL, "https://") || strings.HasPrefix(e.ImageURL, "http://"))
}

func (e itemCreatedEvent) offering() map[string]any {
	off := map[string]any{"source": "inventory", "source_id": e.ID, "type": e.Type, "name": e.Name, "photo_urls": []string{e.ImageURL}}
	if e.SKU != "" {
		off["sku"] = e.SKU
	}
	if e.Description != "" {
		off["description"] = e.Description
	}
	return off
}

func (h *NewItemAdsHandler) Handle(ctx context.Context, msg jetstream.Msg) error {
	ev, err := decodeItemCreated(msg.Data())
	if err != nil {
		return dropPoison(h.logger, msg, "decode inventory.item.created", err)
	}
	return h.process(ctx, ev)
}

func decodeItemCreated(data []byte) (itemCreatedEvent, error) {
	var ev itemCreatedEvent
	if err := json.Unmarshal(eventBody(data), &ev); err != nil {
		return ev, err
	}
	if _, err := uuid.Parse(ev.TenantID); err != nil || ev.ID == "" {
		return ev, fmt.Errorf("item event without tenant or item: %v", err)
	}
	return ev, nil
}

func (h *NewItemAdsHandler) process(ctx context.Context, ev itemCreatedEvent) error {
	if !ev.advertisable() {
		return nil
	}
	tid, err := uuid.Parse(ev.TenantID)
	if err != nil {
		return err
	}
	agents, err := h.db.CustomAgent.Query().
		Where(customagent.TenantID(tid), customagent.IsActive(true), func(s *entsql.Selector) {
			s.Where(entsql.P(func(b *entsql.Builder) {
				b.WriteString(s.C(customagent.FieldMetadata) + "->>'kind' = ").Arg(custom_agents.KindAdAutopilot)
				b.WriteString(" AND " + s.C(customagent.FieldMetadata) + "->'autopilot'->>'new_item_drafts' = 'true'")
			}))
		}).All(ctx)
	if err != nil {
		return err
	}
	for _, a := range agents {
		if err := h.draft(ctx, a, ev); err != nil {
			return err
		}
	}
	return nil
}

// draft makes one agent's draft unless it already made one today, which also covers a redelivered
// event. Items of a bulk import arrive together on several consumers, so the day is claimed first
// and the run row is only the record of it. A draft that fails is recorded on its run and not
// retried, so it cannot charge twice.
func (h *NewItemAdsHandler) draft(ctx context.Context, a *ent.CustomAgent, ev itemCreatedEvent) error {
	now := h.now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if !h.claim(ctx, fmt.Sprintf("marketflow:new-item-ad:%s:%s", a.ID, today.Format("2006-01-02")), 48*time.Hour) {
		return nil
	}
	done, err := h.db.AgentRun.Query().
		Where(agentrun.CustomAgentID(a.ID), agentrun.TriggeredBy(triggeredByNewItem), agentrun.StartedAtGTE(today)).
		Exist(ctx)
	if err != nil || done {
		return err
	}
	run, err := h.db.AgentRun.Create().
		SetTenantID(a.TenantID).SetCustomAgentID(a.ID).SetTriggeredBy(triggeredByNewItem).
		SetTriggerPayload(map[string]any{"item_id": ev.ID, "name": ev.Name}).
		SetStatus("running").SetTrace(map[string]any{}).SetTotalTokens(0).SetToolCallsN(0).SetStartedAt(now).
		Save(ctx)
	if err != nil {
		return err
	}
	res, runErr := h.runner.RunForItem(ctx, a, ev.offering())
	h.runner.record(ctx, a, run.ID, res, runErr)
	return nil
}
