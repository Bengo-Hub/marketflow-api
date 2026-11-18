package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	sharedcache "github.com/Bengo-Hub/cache"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/agentrun"
	"github.com/bengobox/marketflow-api/internal/ent/customagent"
	"github.com/bengobox/marketflow-api/internal/modules/custom_agents"
	"github.com/bengobox/marketflow-api/internal/modules/testimonials"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"
)

// triggeredByTestimonial marks a run started by a newly published testimonial.
const triggeredByTestimonial = "rule:testimonial"

// minTestimonialRating is the lowest rating that starts a draft; an unrated review (0) counts.
const minTestimonialRating = 4

// TestimonialAdsHandler drafts an ad when a good testimonial is published (Sprint 20), the same way
// NewItemAdsHandler does for a new item: an agent opts in (testimonial_drafts), it makes at most
// one draft a day, and the draft waits for the owner's review. The draft is the agent's next
// rotated offering; the script writer reads the tenant's best published reviews, so the new one is
// among its proof.
type TestimonialAdsHandler struct {
	db     *ent.Client
	runner *AutopilotRunner
	logger *zap.Logger
	now    func() time.Time
	claim  func(ctx context.Context, key string, ttl time.Duration) bool
}

func NewTestimonialAdsHandler(db *ent.Client, runner *AutopilotRunner, logger *zap.Logger) *TestimonialAdsHandler {
	return &TestimonialAdsHandler{db: db, runner: runner, logger: logger.Named("testimonial-ads"), now: time.Now, claim: sharedcache.ClaimOnce}
}

func (*TestimonialAdsHandler) Subject() string { return testimonials.TestimonialPublishedTopic }
func (*TestimonialAdsHandler) Durable() string { return "marketflow-testimonial-ads" }

type testimonialPublishedEvent struct {
	TenantID      string `json:"tenant_id"`
	TestimonialID string `json:"testimonial_id"`
	Rating        int    `json:"rating"`
}

// good keeps the draft to reviews worth showing: rated 4 or 5, or unrated.
func (e testimonialPublishedEvent) good() bool {
	return e.Rating == 0 || e.Rating >= minTestimonialRating
}

func (h *TestimonialAdsHandler) Handle(ctx context.Context, msg jetstream.Msg) error {
	ev, err := decodeTestimonialPublished(msg.Data())
	if err != nil {
		return dropPoison(h.logger, msg, "decode "+testimonials.TestimonialPublishedTopic, err)
	}
	return h.process(ctx, ev)
}

func decodeTestimonialPublished(data []byte) (testimonialPublishedEvent, error) {
	var ev testimonialPublishedEvent
	if err := json.Unmarshal(eventBody(data), &ev); err != nil {
		return ev, err
	}
	if _, err := uuid.Parse(ev.TenantID); err != nil || ev.TestimonialID == "" {
		return ev, fmt.Errorf("testimonial event without tenant or testimonial: %v", err)
	}
	return ev, nil
}

func (h *TestimonialAdsHandler) process(ctx context.Context, ev testimonialPublishedEvent) error {
	if !ev.good() {
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
				b.WriteString(" AND " + s.C(customagent.FieldMetadata) + "->'autopilot'->>'testimonial_drafts' = 'true'")
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

// draft makes one agent's draft unless it already made one for a testimonial today, which also
// covers a redelivered event. The day is claimed first, as for new items; a draft that fails is
// recorded on its run and not retried, so it cannot charge twice.
func (h *TestimonialAdsHandler) draft(ctx context.Context, a *ent.CustomAgent, ev testimonialPublishedEvent) error {
	now := h.now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if !h.claim(ctx, fmt.Sprintf("marketflow:testimonial-ad:%s:%s", a.ID, today.Format("2006-01-02")), 48*time.Hour) {
		return nil
	}
	done, err := h.db.AgentRun.Query().
		Where(agentrun.CustomAgentID(a.ID), agentrun.TriggeredBy(triggeredByTestimonial), agentrun.StartedAtGTE(today)).
		Exist(ctx)
	if err != nil || done {
		return err
	}
	run, err := h.db.AgentRun.Create().
		SetTenantID(a.TenantID).SetCustomAgentID(a.ID).SetTriggeredBy(triggeredByTestimonial).
		SetTriggerPayload(map[string]any{"testimonial_id": ev.TestimonialID, "rating": ev.Rating}).
		SetStatus("running").SetTrace(map[string]any{}).SetTotalTokens(0).SetToolCallsN(0).SetStartedAt(now).
		Save(ctx)
	if err != nil {
		return err
	}
	res, runErr := h.runner.RunWithID(ctx, a, run.ID)
	h.runner.record(ctx, a, run.ID, res, runErr)
	return nil
}
