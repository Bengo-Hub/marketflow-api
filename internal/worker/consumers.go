package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/ent/nurturesequence"
	"github.com/bengobox/marketflow-api/internal/modules/activities"
	"github.com/bengobox/marketflow-api/internal/notifications"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"
)

// Handler is a per-subject message handler.
type Handler interface {
	Subject() string
	Durable() string
	Handle(ctx context.Context, msg jetstream.Msg) error
}

// errPoisonDropped is a sentinel returned by handlers after they have already
// settled an UNRECOVERABLE (poison) message via msg.Term(). The Consume
// wrapper treats it as "do not redeliver, do not re-settle": it neither Naks
// nor Acks again. Use dropPoison to terminate + return it.
var errPoisonDropped = errors.New("poison message dropped")

// dropPoison terminates a JetStream message so it is NOT redelivered, logs a
// warning, and returns the errPoisonDropped sentinel. Call this ONLY for
// clearly unrecoverable decode/validation failures (bad JSON, invalid UUID,
// malformed payload that can never succeed) instead of returning a real error
// (which would Nak and waste the MaxDeliver redelivery budget). Transient
// errors (DB, downstream calls, timeouts) must still return a normal error so
// they are Nak'd and retried.
func dropPoison(logger *zap.Logger, msg jetstream.Msg, reason string, err error) error {
	logger.Warn("dropping poison message (unrecoverable)",
		zap.String("subject", msg.Subject()),
		zap.String("reason", reason),
		zap.Error(err),
	)
	// Term tells JetStream to stop redelivery entirely (nats.go v1.48). Ignore
	// the settle error (e.g. already settled) — the drop is logged either way.
	_ = msg.Term()
	return errPoisonDropped
}

// Registry keeps the set of registered consumers.
type Registry struct {
	cfg      *Config
	db       *ent.Client
	logger   *zap.Logger
	handlers []Handler
}

// NewRegistry constructs a Registry with the four core handlers.
// db/rawDB may be nil — handlers that require DB access (lead auto-enrol,
// embedding upsert) degrade to a no-op and log a warning.
func NewRegistry(cfg *Config, db *ent.Client, rawDB *sql.DB, logger *zap.Logger) *Registry {
	notif := notifications.New(notifications.Config{
		BaseURL:   cfg.Notifications.URL,
		APIKey:    cfg.Notifications.APIKey,
		AuthToken: cfg.Notifications.Token,
	})
	activitySvc := activities.NewService(db, logger.Named("activities"))
	agentHTTP := &http.Client{Timeout: 15 * time.Second}
	leadScorer := &LeadScorerAgent{
		db:         db,
		apiBaseURL: cfg.API.MarketFlowAPIURL,
		serviceKey: cfg.API.InternalServiceKey,
		logger:     logger.Named("agent.lead-scorer"),
		hc:         agentHTTP,
	}
	return &Registry{
		cfg:    cfg,
		db:     db,
		logger: logger,
		handlers: []Handler{
			&NurtureStepHandler{cfg: cfg, db: db, notif: notif, logger: logger.Named("nurture")},
			&EmbeddingHandler{cfg: cfg, db: db, rawDB: rawDB, logger: logger.Named("embedding")},
			&ChatCompletedHandler{db: db, logger: logger.Named("audit")},
			&LeadCreatedHandler{cfg: cfg, db: db, logger: logger.Named("lead")},
			&ConversionsHandler{cfg: cfg, db: db, logger: logger.Named("conversions")},
			// CRM cross-service event consumers (Phase D)
			&OrderingOrderCreatedHandler{db: db, svc: activitySvc, logger: logger.Named("crm-ordering")},
			&POSSaleFinalizedHandler{db: db, svc: activitySvc, logger: logger.Named("crm-pos-sale")},
			&POSAppointmentCompletedHandler{db: db, svc: activitySvc, logger: logger.Named("crm-pos-appt")},
			&TreasuryPaymentSucceededHandler{db: db, svc: activitySvc, logger: logger.Named("crm-treasury")},
			// Sprint 10 background AI agents
			leadScorer,
			&ActivityLoggedLeadScorerAgent{LeadScorerAgent: leadScorer},
			&ProactiveFollowUpAgent{
				db:         db,
				apiBaseURL: cfg.API.MarketFlowAPIURL,
				serviceKey: cfg.API.InternalServiceKey,
				logger:     logger.Named("agent.followup"),
				hc:         agentHTTP,
			},
			&DealStaleDetector{
				apiBaseURL: cfg.API.MarketFlowAPIURL,
				serviceKey: cfg.API.InternalServiceKey,
				logger:     logger.Named("agent.deal-stale"),
				hc:         agentHTTP,
			},
			&NurtureSequenceEnhancer{
				db:         db,
				apiBaseURL: cfg.API.MarketFlowAPIURL,
				serviceKey: cfg.API.InternalServiceKey,
				logger:     logger.Named("agent.nurture-enhancer"),
				hc:         agentHTTP,
			},
			&CustomAgentExecutionConsumer{
				db:         db,
				aiBaseURL:  cfg.API.MarketFlowAIURL,
				apiBaseURL: cfg.API.MarketFlowAPIURL,
				serviceKey: cfg.API.InternalServiceKey,
				logger:     logger.Named("agent.custom-execution"),
				hc:         agentHTTP,
			},
		},
	}
}

// RegisterAll binds each handler to its JetStream consumer.
func (r *Registry) RegisterAll(ctx context.Context, js jetstream.JetStream) error {
	for _, h := range r.handlers {
		cons, err := js.CreateOrUpdateConsumer(ctx, r.cfg.NATS.Stream, jetstream.ConsumerConfig{
			Durable:       h.Durable(),
			FilterSubject: h.Subject(),
			AckPolicy:     jetstream.AckExplicitPolicy,
			MaxDeliver:    5,
		})
		if err != nil {
			return err
		}
		hc := h
		if _, err := cons.Consume(func(msg jetstream.Msg) {
			if err := hc.Handle(ctx, msg); err != nil {
				// Poison messages were already terminated by the handler via
				// dropPoison — do NOT Nak (would re-settle / redeliver).
				if errors.Is(err, errPoisonDropped) {
					return
				}
				r.logger.Error("handler error",
					zap.String("subject", msg.Subject()),
					zap.Error(err),
				)
				_ = msg.Nak()
				return
			}
			_ = msg.Ack()
		}); err != nil {
			return err
		}
	}
	return nil
}

// Count returns the number of registered handlers.
func (r *Registry) Count() int { return len(r.handlers) }

// ── Handlers ──────────────────────────────────────────────────────────────

// NurtureStepHandler processes scheduled nurture sequence steps.
type NurtureStepHandler struct {
	cfg    *Config
	db     *ent.Client
	notif  *notifications.Client
	logger *zap.Logger
}

func (h *NurtureStepHandler) Subject() string { return "nurture.step.due" }
func (h *NurtureStepHandler) Durable() string { return "marketflow-worker-nurture" }

func (h *NurtureStepHandler) Handle(ctx context.Context, msg jetstream.Msg) error {
	var envelope struct {
		TenantID     string `json:"tenant_id"`
		SequenceID   string `json:"sequence_id"`
		LeadID       string `json:"lead_id"`
		EnrollmentID string `json:"enrollment_id"`
		StepIndex    int    `json:"step_index"`
	}
	if err := json.Unmarshal(msg.Data(), &envelope); err != nil {
		return dropPoison(h.logger, msg, "decode envelope", err)
	}
	if h.db == nil {
		return fmt.Errorf("nurture dispatch requires DB — POSTGRES_URL not configured")
	}
	enrollmentID, err := uuid.Parse(envelope.EnrollmentID)
	if err != nil {
		return dropPoison(h.logger, msg, "bad enrollment_id", err)
	}

	enrollment, err := h.db.NurtureEnrollment.Get(ctx, enrollmentID)
	if err != nil {
		return fmt.Errorf("load enrollment: %w", err)
	}
	if enrollment.Status != "active" {
		h.logger.Debug("skipping non-active enrollment",
			zap.String("enrollment", enrollmentID.String()),
			zap.String("status", enrollment.Status))
		return nil
	}

	seq, err := h.db.NurtureSequence.Get(ctx, enrollment.SequenceID)
	if err != nil {
		return fmt.Errorf("load sequence: %w", err)
	}
	if enrollment.CurrentStep >= len(seq.Steps) {
		return h.completeEnrollment(ctx, enrollment, "completed")
	}
	stepDef := seq.Steps[enrollment.CurrentStep]

	// Resolve recipient from the lead row. Channel selection is a step property;
	// email/phone come from the lead. Fall back to the sequence default.
	var recipient string
	var leadRow *ent.Lead
	if enrollment.LeadID != nil {
		ld, err := h.db.Lead.Query().Where(lead.IDEQ(*enrollment.LeadID)).Only(ctx)
		if err == nil {
			leadRow = ld
			switch asString(stepDef, "channel") {
			case "whatsapp", "sms":
				if ld.Phone != nil {
					recipient = *ld.Phone
				}
			default: // email
				if ld.Email != nil {
					recipient = *ld.Email
				}
			}
		}
	}
	channel := asString(stepDef, "channel")
	if channel == "" {
		channel = "email"
	}
	if recipient == "" {
		h.logger.Warn("no recipient on lead; marking enrollment errored",
			zap.String("enrollment", enrollmentID.String()), zap.String("channel", channel))
		return h.completeEnrollment(ctx, enrollment, "errored")
	}

	// Resolve {{first_name}} / {{product_link}} / etc. against the lead +
	// tenant state BEFORE handing off to notifications-api. notifications-api
	// may also render templates, but MarketFlow is the source of truth for
	// lead-specific placeholders so we substitute here.
	vars := resolveNurtureVariables(ctx, h.db, asMap(stepDef, "variables"), leadRow, enrollment.TenantID)
	renderedBody := renderTemplate(asString(stepDef, "body"), vars)
	renderedSubject := renderTemplate(asString(stepDef, "subject"), vars)

	idemKey := fmt.Sprintf("nurture-%s-%d", enrollmentID, enrollment.CurrentStep)
	payload := notifications.Message{
		Channel:    channel,
		TemplateID: asString(stepDef, "template"),
		Recipient:  recipient,
		Subject:    renderedSubject,
		Body:       renderedBody,
		Data:       vars,
		Metadata: map[string]string{
			"source":        "marketflow-worker",
			"sequence_id":   enrollment.SequenceID.String(),
			"enrollment_id": enrollmentID.String(),
			"step_index":    fmt.Sprintf("%d", enrollment.CurrentStep),
		},
		IdempotencyKey: idemKey,
	}

	h.logger.Info("dispatching nurture step",
		zap.String("tenant", enrollment.TenantID.String()),
		zap.String("channel", channel),
		zap.Int("step", enrollment.CurrentStep),
		zap.String("idem", idemKey),
	)
	if err := h.notif.Send(ctx, enrollment.TenantID.String(), payload); err != nil {
		return fmt.Errorf("notifications send: %w", err)
	}

	// Advance: move to next step, compute its next_step_at from delay_hours,
	// or mark completed when no steps remain.
	nextIdx := enrollment.CurrentStep + 1
	update := h.db.NurtureEnrollment.UpdateOneID(enrollmentID).
		SetCurrentStep(nextIdx).
		SetLastStepAt(time.Now().UTC())
	if nextIdx >= len(seq.Steps) {
		update.SetStatus("completed").
			SetCompletedAt(time.Now().UTC()).
			SetExitReason("completed").
			ClearNextStepAt()
		if _, err := update.Save(ctx); err != nil {
			return fmt.Errorf("finalise enrollment: %w", err)
		}
		// Best-effort counter bump.
		_, _ = h.db.NurtureSequence.UpdateOneID(seq.ID).AddTotalCompleted(1).Save(ctx)
		return nil
	}
	nextStep := seq.Steps[nextIdx]
	delayHours := asInt(nextStep, "delay_hours")
	if delayHours < 0 {
		delayHours = 0
	}
	update.SetNextStepAt(time.Now().UTC().Add(time.Duration(delayHours) * time.Hour))
	if _, err := update.Save(ctx); err != nil {
		return fmt.Errorf("advance enrollment: %w", err)
	}
	return nil
}

// completeEnrollment marks an enrollment terminal with the given reason.
func (h *NurtureStepHandler) completeEnrollment(ctx context.Context, e *ent.NurtureEnrollment, reason string) error {
	_, err := h.db.NurtureEnrollment.UpdateOneID(e.ID).
		SetStatus(reason).
		SetCompletedAt(time.Now().UTC()).
		SetExitReason(reason).
		ClearNextStepAt().
		Save(ctx)
	return err
}

// asString / asInt / asMap are tolerant readers for the sequence.steps[] JSON.
func asString(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}
func asInt(m map[string]any, k string) int {
	switch v := m[k].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return 0
}
func asMap(m map[string]any, k string) map[string]any {
	if v, ok := m[k].(map[string]any); ok {
		return v
	}
	return nil
}

// EmbeddingHandler runs the KB embedding pipeline: fetch entry → embed via Ollama → upsert.
type EmbeddingHandler struct {
	cfg    *Config
	db     *ent.Client
	rawDB  *sql.DB // needed for pgvector upsert (Ent doesn't model the vector column)
	logger *zap.Logger
}

func (h *EmbeddingHandler) Subject() string { return "embedding.requested" }
func (h *EmbeddingHandler) Durable() string { return "marketflow-worker-embedding" }

func (h *EmbeddingHandler) Handle(ctx context.Context, msg jetstream.Msg) error {
	var req struct {
		TenantID string `json:"tenant_id"`
		EntryID  string `json:"entry_id"`
	}
	if err := json.Unmarshal(msg.Data(), &req); err != nil {
		return dropPoison(h.logger, msg, "decode embedding request", err)
	}
	if h.db == nil {
		h.logger.Warn("embedding.requested but no DB — skipping", zap.String("entry", req.EntryID))
		return nil
	}
	entryID, err := uuid.Parse(req.EntryID)
	if err != nil {
		return dropPoison(h.logger, msg, "bad entry_id", err)
	}
	entry, err := h.db.KnowledgeBaseEntry.Get(ctx, entryID)
	if err != nil {
		return fmt.Errorf("load entry: %w", err)
	}

	// Chunk long content on paragraph boundaries; keep under ~1k chars per chunk
	// so nomic-embed-text gets clean embeddings.
	chunks := chunkText(entry.Content, 900)

	// Embed via Ollama and upsert each chunk.
	upserted := 0
	for i, chunk := range chunks {
		vec, err := ollamaEmbed(ctx, h.cfg.API.OllamaURL, h.cfg.API.EmbeddingModel, chunk)
		if err != nil {
			h.logger.Error("ollama embed failed",
				zap.String("entry", entryID.String()), zap.Int("chunk", i), zap.Error(err))
			return err
		}
		if err := upsertEmbedding(ctx, h.rawDB, entry.TenantID, entryID, i, chunk, entry.Category, entry.Title, vec); err != nil {
			h.logger.Error("upsert embedding failed",
				zap.String("entry", entryID.String()), zap.Int("chunk", i), zap.Error(err))
			return err
		}
		upserted++
	}

	if _, err := h.db.KnowledgeBaseEntry.UpdateOneID(entryID).
		SetEmbedded(true).
		SetChunkCount(upserted).
		Save(ctx); err != nil {
		h.logger.Warn("update entry embedded flag", zap.Error(err))
	}
	h.logger.Info("embedding done", zap.String("entry", entryID.String()), zap.Int("chunks", upserted))
	return nil
}

// ChatCompletedHandler writes an AuditLog row for every completed chat turn so
// tenants get a compliance trail + platform gets usage visibility. Payload is
// kept in full for forensics; the UI truncates for display.
type ChatCompletedHandler struct {
	db     *ent.Client
	logger *zap.Logger
}

func (h *ChatCompletedHandler) Subject() string { return "chat.completed" }
func (h *ChatCompletedHandler) Durable() string { return "marketflow-worker-audit" }

func (h *ChatCompletedHandler) Handle(ctx context.Context, msg jetstream.Msg) error {
	var ev struct {
		TenantID   string         `json:"tenant_id"`
		SessionID  string         `json:"session_id"`
		LeadID     string         `json:"lead_id,omitempty"`
		Intent     string         `json:"intent,omitempty"`
		Model      string         `json:"model,omitempty"`
		Tokens     int            `json:"tokens,omitempty"`
		Question   string         `json:"question,omitempty"`
		ActorEmail string         `json:"actor_email,omitempty"`
		Raw        map[string]any `json:"raw,omitempty"`
	}
	if err := json.Unmarshal(msg.Data(), &ev); err != nil {
		h.logger.Warn("chat.completed decode", zap.Error(err))
	}
	if h.db == nil {
		// DB optional for local runs; log only.
		h.logger.Info("chat.completed (no db)", zap.ByteString("payload", msg.Data()))
		return nil
	}
	payload := map[string]any{
		"session_id": ev.SessionID,
		"lead_id":    ev.LeadID,
		"intent":     ev.Intent,
		"model":      ev.Model,
		"tokens":     ev.Tokens,
	}
	if ev.Question != "" {
		payload["question_preview"] = ev.Question
	}
	if ev.Raw != nil {
		payload["raw"] = ev.Raw
	}
	b := h.db.AuditLog.Create().
		SetScope("tenant").
		SetEvent("chat.completed").
		SetPayload(payload)
	if tID, err := uuid.Parse(ev.TenantID); err == nil {
		b.SetTenantID(tID)
	}
	if ev.ActorEmail != "" {
		b.SetActor(ev.ActorEmail)
	}
	if sID, err := uuid.Parse(ev.SessionID); err == nil {
		b.SetSubjectType("chat_session").SetSubjectID(sID)
	}
	if _, err := b.Save(ctx); err != nil {
		return fmt.Errorf("audit insert: %w", err)
	}
	return nil
}

// LeadCreatedHandler auto-starts the tenant's default nurture sequence for new leads.
// Picks the active sequence whose trigger_event == "lead.created" and whose
// lead_type_filter (if set) matches the lead's type.
type LeadCreatedHandler struct {
	cfg    *Config
	db     *ent.Client
	logger *zap.Logger
}

func (h *LeadCreatedHandler) Subject() string { return "lead.created" }
func (h *LeadCreatedHandler) Durable() string { return "marketflow-worker-lead" }

func (h *LeadCreatedHandler) Handle(ctx context.Context, msg jetstream.Msg) error {
	var ev struct {
		TenantID string `json:"tenant_id"`
		LeadID   string `json:"lead_id"`
		LeadType string `json:"lead_type"`
		Source   string `json:"source"`
	}
	if err := json.Unmarshal(msg.Data(), &ev); err != nil {
		return dropPoison(h.logger, msg, "decode lead.created", err)
	}
	if h.db == nil {
		h.logger.Warn("lead.created received but no DB client configured — skipping auto-enrol",
			zap.String("tenant", ev.TenantID), zap.String("lead", ev.LeadID))
		return nil
	}
	tenantUUID, err := uuid.Parse(ev.TenantID)
	if err != nil {
		return dropPoison(h.logger, msg, "invalid tenant_id", err)
	}
	leadUUID, err := uuid.Parse(ev.LeadID)
	if err != nil {
		return dropPoison(h.logger, msg, "invalid lead_id", err)
	}

	// Match the first active sequence with trigger_event="lead.created" whose
	// lead_type_filter is either nil or equals the lead's type.
	q := h.db.NurtureSequence.Query().Where(
		nurturesequence.TenantIDEQ(tenantUUID),
		nurturesequence.TriggerEventEQ("lead.created"),
		nurturesequence.ActiveEQ(true),
	)
	seqs, err := q.All(ctx)
	if err != nil {
		return fmt.Errorf("query sequences: %w", err)
	}
	var chosen *ent.NurtureSequence
	for _, s := range seqs {
		if s.LeadTypeFilter == nil || *s.LeadTypeFilter == "" || *s.LeadTypeFilter == ev.LeadType {
			chosen = s
			break
		}
	}
	if chosen == nil {
		h.logger.Debug("no matching nurture sequence",
			zap.String("tenant", ev.TenantID), zap.String("lead_type", ev.LeadType))
		return nil
	}

	now := time.Now().UTC()
	enrollment, err := h.db.NurtureEnrollment.Create().
		SetTenantID(tenantUUID).
		SetSequenceID(chosen.ID).
		SetLeadID(leadUUID).
		SetCurrentStep(0).
		SetStatus("active").
		SetNextStepAt(now). // fire first step ASAP
		SetMetadata(map[string]any{"source": ev.Source, "auto_enrolled": true}).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("create enrollment: %w", err)
	}

	// Bump sequence counter (best-effort; not critical if it fails).
	_, _ = h.db.NurtureSequence.UpdateOneID(chosen.ID).
		AddTotalEnrolled(1).
		Save(ctx)

	h.logger.Info("lead auto-enrolled",
		zap.String("tenant", ev.TenantID),
		zap.String("lead", ev.LeadID),
		zap.String("sequence", chosen.ID.String()),
		zap.String("enrollment", enrollment.ID.String()),
	)
	return nil
}
