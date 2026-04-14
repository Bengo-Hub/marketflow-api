package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/nurturesequence"
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

// Registry keeps the set of registered consumers.
type Registry struct {
	cfg      *Config
	db       *ent.Client
	logger   *zap.Logger
	handlers []Handler
}

// NewRegistry constructs a Registry with the four core handlers.
// db may be nil — handlers that require DB access (lead auto-enrol) degrade
// to a no-op and log a warning.
func NewRegistry(cfg *Config, db *ent.Client, logger *zap.Logger) *Registry {
	notif := notifications.New(notifications.Config{
		BaseURL:   cfg.Notifications.URL,
		APIKey:    cfg.Notifications.APIKey,
		AuthToken: cfg.Notifications.Token,
	})
	return &Registry{
		cfg:    cfg,
		db:     db,
		logger: logger,
		handlers: []Handler{
			&NurtureStepHandler{cfg: cfg, db: db, notif: notif, logger: logger.Named("nurture")},
			&EmbeddingHandler{cfg: cfg, logger: logger.Named("embedding")},
			&ChatCompletedHandler{logger: logger.Named("audit")},
			&LeadCreatedHandler{cfg: cfg, db: db, logger: logger.Named("lead")},
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
	var step struct {
		TenantID     string         `json:"tenant_id"`
		SequenceID   string         `json:"sequence_id"`
		LeadID       string         `json:"lead_id"`
		EnrollmentID string         `json:"enrollment_id"`
		StepIndex    int            `json:"step_index"`
		Channel      string         `json:"channel"`
		Template     string         `json:"template"`
		Recipient    string         `json:"recipient"`
		Subject      string         `json:"subject,omitempty"`
		Body         string         `json:"body,omitempty"`
		Variables    map[string]any `json:"variables,omitempty"`
	}
	if err := json.Unmarshal(msg.Data(), &step); err != nil {
		return err
	}
	if step.TenantID == "" || step.Channel == "" || step.Recipient == "" {
		return fmt.Errorf("invalid step: tenant=%q channel=%q recipient=%q",
			step.TenantID, step.Channel, step.Recipient)
	}

	idemKey := fmt.Sprintf("nurture-%s-%d", step.EnrollmentID, step.StepIndex)
	payload := notifications.Message{
		Channel:    step.Channel,
		TemplateID: step.Template,
		Recipient:  step.Recipient,
		Subject:    step.Subject,
		Body:       step.Body,
		Data:       step.Variables,
		Metadata: map[string]string{
			"source":        "marketflow-worker",
			"sequence_id":   step.SequenceID,
			"enrollment_id": step.EnrollmentID,
			"lead_id":       step.LeadID,
			"step_index":    fmt.Sprintf("%d", step.StepIndex),
		},
		IdempotencyKey: idemKey,
	}

	h.logger.Info("dispatching nurture step",
		zap.String("tenant", step.TenantID),
		zap.String("channel", step.Channel),
		zap.Int("step", step.StepIndex),
		zap.String("idem", idemKey),
	)
	if err := h.notif.Send(ctx, step.TenantID, payload); err != nil {
		return fmt.Errorf("notifications send: %w", err)
	}
	// TODO: call marketflow-api to advance enrollment.current_step + set next_step_at.
	return nil
}

// EmbeddingHandler runs the KB embedding pipeline: fetch entry → embed via Ollama → upsert.
type EmbeddingHandler struct {
	cfg    *Config
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
		return err
	}
	h.logger.Info("embedding requested", zap.String("entry", req.EntryID))
	// TODO: pull entry → Ollama /api/embeddings → UPSERT knowledge_base_embeddings.
	return nil
}

// ChatCompletedHandler logs completed chat sessions for audit / usage metrics.
type ChatCompletedHandler struct {
	logger *zap.Logger
}

func (h *ChatCompletedHandler) Subject() string { return "chat.completed" }
func (h *ChatCompletedHandler) Durable() string { return "marketflow-worker-audit" }

func (h *ChatCompletedHandler) Handle(ctx context.Context, msg jetstream.Msg) error {
	h.logger.Info("chat.completed", zap.ByteString("payload", msg.Data()))
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
		return err
	}
	if h.db == nil {
		h.logger.Warn("lead.created received but no DB client configured — skipping auto-enrol",
			zap.String("tenant", ev.TenantID), zap.String("lead", ev.LeadID))
		return nil
	}
	tenantUUID, err := uuid.Parse(ev.TenantID)
	if err != nil {
		return fmt.Errorf("invalid tenant_id: %w", err)
	}
	leadUUID, err := uuid.Parse(ev.LeadID)
	if err != nil {
		return fmt.Errorf("invalid lead_id: %w", err)
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
