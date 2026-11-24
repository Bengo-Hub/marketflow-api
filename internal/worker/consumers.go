package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/knowledgebaseentry"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/integrations/meta"
	"github.com/bengobox/marketflow-api/internal/modules/activities"
	"github.com/bengobox/marketflow-api/internal/modules/contacts"
	"github.com/bengobox/marketflow-api/internal/modules/custom_agents"
	"github.com/bengobox/marketflow-api/internal/notifications"
	"github.com/bengobox/marketflow-api/internal/platform/leadresponse"
	"github.com/bengobox/marketflow-api/internal/platform/reqid"
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

// finalFailureRecorder is implemented by a handler that must keep a durable record when its
// message is given up on after the last delivery (an erasure request, for one), instead of only
// the error log line.
type finalFailureRecorder interface {
	OnFinalFailure(ctx context.Context, msg jetstream.Msg)
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
	// autopilotLapsed is bound to the autopublish pipeline once it is built (AutopilotLapsed).
	autopilotLapsed *AutopilotLapsedHook
}

// AutopilotLapsed is the hook the autopilot runs call when the plan or terms no longer allow the
// veto-window mode; bind it with Set once the autopublish pipeline exists.
func (r *Registry) AutopilotLapsed() *AutopilotLapsedHook { return r.autopilotLapsed }

// NewRegistry constructs a Registry with the four core handlers.
// db/rawDB may be nil — handlers that require DB access (lead auto-enrol,
// embedding upsert) degrade to a no-op and log a warning.
//
// keys decrypts provider-setting tokens (Meta CAPI, Google Ads) for the conversions handler.
func NewRegistry(cfg *Config, db *ent.Client, rawDB *sql.DB, keys *encryption.KeyProvider, logger *zap.Logger) *Registry {
	notif := notifications.New(notifications.Config{
		BaseURL:   cfg.Notifications.URL,
		APIKey:    cfg.Notifications.APIKey,
		AuthToken: cfg.Notifications.Token,
	})
	activitySvc := activities.NewService(db, logger.Named("activities"))
	agentHTTP := reqid.Client(15 * time.Second)
	leadScorer := &LeadScorerAgent{
		db:         db,
		apiBaseURL: cfg.API.MarketFlowAPIURL,
		serviceKey: cfg.API.InternalServiceKey,
		logger:     logger.Named("agent.lead-scorer"),
		hc:         agentHTTP,
	}
	lapsed := &AutopilotLapsedHook{}
	return &Registry{
		cfg:             cfg,
		db:              db,
		logger:          logger,
		autopilotLapsed: lapsed,
		handlers: []Handler{
			&NurtureStepHandler{cfg: cfg, db: db, notif: notif, logger: logger.Named("nurture")},
			// How each nurture message did, from notifications-api (sequence analytics)
			&NurtureEngagementHandler{db: db, logger: logger.Named("nurture-engagement")},
			&EmbeddingHandler{cfg: cfg, db: db, rawDB: rawDB, logger: logger.Named("embedding")},
			&ChatCompletedHandler{db: db, logger: logger.Named("audit")},
			&LeadCreatedHandler{cfg: cfg, db: db, logger: logger.Named("lead")},
			&LeadResubmittedHandler{db: db, logger: logger.Named("lead-resubmitted")},
			&ConversionsHandler{cfg: cfg, db: db, keys: keys, logger: logger.Named("conversions")},
			&MetaLeadEnricher{
				db: db,
				// Lead reads use the Page token our app issued, so they carry appsecret_proof.
				hc: &http.Client{Timeout: 15 * time.Second,
					Transport: meta.NewProofTransport(nil, meta.PlatformSecret(db, keys, os.Getenv("MF_META_APP_SECRET")))},
				tokens:   NewMetaTokenResolver(db, keys, logger),
				contacts: contacts.NewService(db, logger),
				logger:   logger.Named("meta-lead-enricher"),
			},
			// CRM cross-service event consumers (Phase D)
			&OrderingOrderCreatedHandler{db: db, svc: activitySvc, logger: logger.Named("crm-ordering")},
			&POSSaleFinalizedHandler{db: db, svc: activitySvc, logger: logger.Named("crm-pos-sale")},
			&POSAppointmentCompletedHandler{db: db, svc: activitySvc, logger: logger.Named("crm-pos-appt")},
			&TreasuryPaymentSucceededHandler{db: db, svc: activitySvc, logger: logger.Named("crm-treasury")},
			// Timeline entries other services request (ticketing-api: ticket opened and resolved)
			&CRMActivityRequestedHandler{db: db, svc: activitySvc, logger: logger.Named("crm-activity-requested")},
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
				autopilot: NewAutopilotRunner(db, notif, cfg.API.PortalURL, logger).WithForecast(custom_agents.NewCreditForecaster(cfg.API.MarketFlowAIURL, cfg.API.InternalServiceKey)).
					WithAutoPublishGate(AutopilotGate(cfg, db)).WithLapsedHook(lapsed).WithAdFunds(AdFunds(db, keys)).
					WithVideoFeatures(VideoFeatures(cfg)),
			},
		},
	}
}

// streamFor maps a handler's subject to the JetStream stream that OWNS it. Cross-service
// subjects (ordering./pos./treasury.) live in those services' streams — NATS forbids
// subject overlap between streams, so binding them to the marketflow stream can never
// work. Marketflow-internal work signals (nurture./crm./embedding./chat./lead.) live in
// the marketflow stream (ensured by cmd/worker with those extra subject bindings).
func (r *Registry) streamFor(subject string) string {
	switch {
	case strings.HasPrefix(subject, "ordering."):
		return "ordering"
	case strings.HasPrefix(subject, "pos."):
		return "pos"
	case strings.HasPrefix(subject, "treasury."):
		return "treasury"
	case strings.HasPrefix(subject, "notifications."):
		return "notifications"
	case strings.HasPrefix(subject, "inventory."):
		return "inventory"
	case strings.HasPrefix(subject, "maskani."):
		return "maskani"
	default:
		return r.cfg.NATS.Stream
	}
}

// RegisterAll binds each handler to its JetStream consumer on the subject's owning stream.
func (r *Registry) RegisterAll(ctx context.Context, js jetstream.JetStream) error {
	for _, h := range r.handlers {
		cons, err := js.CreateOrUpdateConsumer(ctx, r.streamFor(h.Subject()), jetstream.ConsumerConfig{
			Durable:       h.Durable(),
			FilterSubject: h.Subject(),
			AckPolicy:     jetstream.AckExplicitPolicy,
			MaxDeliver:    consumerMaxDeliver,
			// Above the slowest handler's own bounds (an embedding call alone may take 30s), so
			// a slow success is never redelivered and processed twice.
			AckWait: consumerAckWait,
		})
		if err != nil {
			return err
		}
		hc := h
		if _, err := cons.Consume(func(msg jetstream.Msg) {
			if err := hc.Handle(reqid.WithEvent(ctx, msg.Headers(), msg.Data()), msg); err != nil {
				// Poison messages were already terminated by the handler via
				// dropPoison — do NOT Nak (would re-settle / redeliver).
				if errors.Is(err, errPoisonDropped) {
					return
				}
				delivered := uint64(1)
				if md, merr := msg.Metadata(); merr == nil && md != nil {
					delivered = md.NumDelivered
				}
				if delivered >= consumerMaxDeliver {
					// Out of retries: say so loudly instead of letting the server drop it quietly.
					r.logger.Error("handler failed on the last delivery; message dropped",
						zap.String("subject", msg.Subject()), zap.String("durable", hc.Durable()),
						zap.Uint64("delivered", delivered), zap.Error(err))
					if ff, ok := hc.(finalFailureRecorder); ok {
						ff.OnFinalFailure(ctx, msg)
					}
					_ = msg.Term()
					return
				}
				delay := retryDelay(delivered)
				r.logger.Warn("handler error, retrying later",
					zap.String("subject", msg.Subject()), zap.Uint64("delivered", delivered),
					zap.Duration("retry_in", delay), zap.Error(err))
				_ = msg.NakWithDelay(delay)
				return
			}
			_ = msg.Ack()
		}); err != nil {
			return err
		}
	}
	return nil
}

const (
	consumerMaxDeliver = 5
	consumerAckWait    = 2 * time.Minute
)

// consumerRetryDelays spread a failing message's retries over about 16 minutes. An immediate
// Nak would spend every delivery within seconds while a downstream (marketflow-ai, Ollama,
// Meta) is briefly down, and the message would be lost for an outage that heals in a minute.
var consumerRetryDelays = []time.Duration{10 * time.Second, time.Minute, 5 * time.Minute, 10 * time.Minute}

// retryDelay is the wait before redelivering a message that failed on delivery n (1-based).
func retryDelay(n uint64) time.Duration {
	if n == 0 {
		n = 1
	}
	if int(n) > len(consumerRetryDelays) {
		return consumerRetryDelays[len(consumerRetryDelays)-1]
	}
	return consumerRetryDelays[n-1]
}

// Add registers extra handlers built outside NewRegistry (they need clients the
// registry does not hold, such as a NATS connection). Call it before RegisterAll.
func (r *Registry) Add(hs ...Handler) { r.handlers = append(r.handlers, hs...) }

// SetNurtureKick hands the nurture scheduler's Kick to the lead.created handler. Call it before
// RegisterAll, so no consumer goroutine reads the field while it is set.
func (r *Registry) SetNurtureKick(kick func()) {
	for _, h := range r.handlers {
		switch lh := h.(type) {
		case *LeadCreatedHandler:
			lh.kick = kick
		case *LeadResubmittedHandler:
			lh.kick = kick
		}
	}
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

	// A lead follow-up stops once a person has the conversation (owner, 2026-11-24): the lead
	// replied, booked a meeting or moved past new, or the tenant switched its default off.
	if seq.TriggerEvent == leadresponse.TriggerLeadCreated && enrollment.LeadID != nil {
		reason, err := followUpStopReason(ctx, h.db, enrollment, seq)
		if err != nil {
			return fmt.Errorf("follow-up stop check: %w", err)
		}
		if reason != "" {
			h.logger.Info("lead follow-up stopped", zap.String("enrollment", enrollmentID.String()), zap.String("reason", reason))
			return h.stopEnrollment(ctx, enrollment, reason)
		}
	}
	// An optional step that cannot reach the lead is skipped, not moved to another channel and
	// not the end of the sequence.
	optional, _ := stepDef["optional"].(bool)

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
	// WhatsApp only with the lead's own consent (owner, 2026-11-03; Kenya Data Protection Act):
	// the "OK to contact me on WhatsApp" tick on forms, stored as metadata.whatsapp_consent.
	// Without it the step goes by email when the lead has one, else it is skipped.
	if channel == "whatsapp" && leadRow != nil && !whatsAppConsent(leadRow) {
		if optional {
			return h.skipStep(ctx, enrollment, seq, "no WhatsApp consent")
		}
		if leadRow.Email != nil && *leadRow.Email != "" {
			channel, recipient = "email", *leadRow.Email
		} else {
			// Nothing may reach this lead: end the enrollment as cancelled with its own reason,
			// not as an error.
			h.logger.Info("nurture cancelled: no WhatsApp consent and no email",
				zap.String("enrollment", enrollmentID.String()), zap.Int("step", enrollment.CurrentStep))
			_, err := h.db.NurtureEnrollment.UpdateOneID(enrollment.ID).
				SetStatus("cancelled").
				SetExitReason("no_contact_consent").
				SetCompletedAt(time.Now().UTC()).
				ClearNextStepAt().
				Save(ctx)
			return err
		}
	}
	// Opt-outs (STOP replies, unsubscribe links) are kept on contacts; honour them here too.
	if leadRow != nil && recipient != "" {
		email, phone := "", ""
		if leadRow.Email != nil {
			email = *leadRow.Email
		}
		if leadRow.Phone != nil {
			phone = *leadRow.Phone
		}
		emailOut, phoneOut := contactOptOuts(ctx, h.db, enrollment.TenantID, email, phone)
		ch, to, ok := nurtureChannelAfterOptOut(channel, recipient, email, emailOut, phoneOut)
		if optional && ok && ch != channel {
			// An optional step never moves to another channel; the other steps cover it.
			return h.skipStep(ctx, enrollment, seq, "opted out of "+channel)
		}
		if !ok {
			h.logger.Info("nurture cancelled: the person opted out",
				zap.String("enrollment", enrollmentID.String()), zap.String("channel", channel))
			_, err := h.db.NurtureEnrollment.UpdateOneID(enrollment.ID).
				SetStatus("cancelled").
				SetExitReason("unsubscribed").
				SetCompletedAt(time.Now().UTC()).
				ClearNextStepAt().
				Save(ctx)
			return err
		}
		channel, recipient = ch, to
	}
	if recipient == "" && optional {
		return h.skipStep(ctx, enrollment, seq, "no "+channel+" address")
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
	if channel != "email" {
		// The branding keys only mean something to the email layout.
		for _, k := range emailBrandKeys {
			delete(vars, k)
		}
	}
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
	// A WhatsApp step naming an approved Meta template goes as that template from the tenant's
	// own number: the only way to write to a lead outside the 24-hour window.
	if channel == "whatsapp" {
		payload.Template = stepWhatsAppTemplate(stepDef, vars)
	}

	h.logger.Info("dispatching nurture step",
		zap.String("tenant", enrollment.TenantID.String()),
		zap.String("channel", channel),
		zap.Int("step", enrollment.CurrentStep),
		zap.String("idem", idemKey),
	)
	if err := h.notif.Send(ctx, enrollment.TenantID.String(), payload); err != nil {
		if optional {
			// A template still in Meta review or a tenant without its own WhatsApp number: the
			// other steps still go.
			h.logger.Warn("optional nurture step not sent; skipped", zap.String("enrollment", enrollmentID.String()),
				zap.String("channel", channel), zap.Error(err))
			return h.skipStep(ctx, enrollment, seq, "send failed")
		}
		return fmt.Errorf("notifications send: %w", err)
	}
	// The message on the lead's timeline (Sprint 23 L29). Best effort: it was already sent.
	if leadRow != nil {
		if err := logNurtureActivity(ctx, h.db, enrollment.TenantID, leadRow.ID, channel, renderedSubject, renderedBody, idemKey,
			map[string]any{"sequence_id": enrollment.SequenceID.String(), "enrollment_id": enrollmentID.String(), "step_index": enrollment.CurrentStep}); err != nil {
			h.logger.Warn("log nurture activity", zap.String("lead", leadRow.ID.String()), zap.Error(err))
		}
	}
	// The first automated message a lead gets is metadata.first_reply_at: lead created to first
	// reply is the speed-to-lead figure (Sprint 23). contacted_at stays the person's own "contacted"
	// status change. One atomic UPDATE that sets the key only when it is absent.
	if leadRow != nil {
		if err := recordFirstReply(ctx, h.db, leadRow.ID, time.Now().UTC()); err != nil {
			h.logger.Warn("record first reply", zap.String("lead", leadRow.ID.String()), zap.Error(err))
		}
	}

	return h.advance(ctx, enrollment, seq)
}

// skipStep moves past an optional step the lead cannot receive, without sending it.
func (h *NurtureStepHandler) skipStep(ctx context.Context, e *ent.NurtureEnrollment, seq *ent.NurtureSequence, why string) error {
	h.logger.Info("optional nurture step skipped", zap.String("enrollment", e.ID.String()),
		zap.Int("step", e.CurrentStep), zap.String("why", why))
	return h.advance(ctx, e, seq)
}

// advance moves the enrollment to its next step, timed by that step's delay_hours, or completes
// it when no steps remain.
func (h *NurtureStepHandler) advance(ctx context.Context, enrollment *ent.NurtureEnrollment, seq *ent.NurtureSequence) error {
	enrollmentID := enrollment.ID
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

// stopEnrollment ends a lead follow-up early, as cancelled with the reason it stopped.
func (h *NurtureStepHandler) stopEnrollment(ctx context.Context, e *ent.NurtureEnrollment, reason string) error {
	_, err := h.db.NurtureEnrollment.UpdateOneID(e.ID).
		SetStatus("cancelled").
		SetExitReason(reason).
		SetCompletedAt(time.Now().UTC()).
		ClearNextStepAt().
		Save(ctx)
	return err
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

// NewEmbeddingHandler builds the handler for use outside the registry (the embed sweep).
func NewEmbeddingHandler(cfg *Config, db *ent.Client, rawDB *sql.DB, logger *zap.Logger) *EmbeddingHandler {
	return &EmbeddingHandler{cfg: cfg, db: db, rawDB: rawDB, logger: logger.Named("embedding")}
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
	return h.embedEntry(ctx, entry)
}

// embedEntry writes an entry's search vectors. The event above and the embed sweep both use it.
func (h *EmbeddingHandler) embedEntry(ctx context.Context, entry *ent.KnowledgeBaseEntry) error {
	entryID := entry.ID
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
	// An entry that got shorter must not keep answering from its old tail.
	if _, err := h.rawDB.ExecContext(ctx, `DELETE FROM knowledge_base_embeddings WHERE entry_id = $1 AND chunk_index >= $2`, entryID, upserted); err != nil {
		return fmt.Errorf("trim old chunks: %w", err)
	}

	// Only if the text is still what was embedded; an edit meanwhile stays queued for the sweep.
	if _, err := h.db.KnowledgeBaseEntry.Update().
		Where(knowledgebaseentry.ID(entryID), knowledgebaseentry.UpdatedAtEQ(entry.UpdatedAt)).
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
	if err := json.Unmarshal(eventBody(msg.Data()), &ev); err != nil {
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
	// kick asks the nurture scheduler for a tick now, so a first step with no delay goes out
	// within seconds of lead.created (Sprint 23's 60-second follow-up). Nil means wait for the
	// next interval.
	kick func()
}

func (h *LeadCreatedHandler) Subject() string { return "lead.created" }
func (h *LeadCreatedHandler) Durable() string { return "marketflow-worker-lead" }

func (h *LeadCreatedHandler) Handle(ctx context.Context, msg jetstream.Msg) error {
	ev, err := decodeLeadCreated(msg.Data())
	if err != nil {
		return dropPoison(h.logger, msg, "decode lead.created", err)
	}
	if h.db == nil {
		h.logger.Warn("lead.created received but no DB client configured — skipping auto-enrol",
			zap.String("tenant", ev.TenantID.String()), zap.String("lead", ev.LeadID.String()))
		return nil
	}
	return enrolLead(ctx, h.db, h.logger, h.kick, ev, false)
}

// recordFirstReply merges metadata.first_reply_at into a lead once, in one UPDATE (metadata ||
// patch, as tenant metastore does), so it never overwrites other metadata keys and a later step
// never moves the first reply. jsonb_exists is used, not the ? operator, which the builder would
// read as a placeholder.
func recordFirstReply(ctx context.Context, db *ent.Client, leadID uuid.UUID, at time.Time) error {
	patch, _ := json.Marshal(map[string]any{"first_reply_at": at.Format(time.RFC3339)})
	value := entsql.ExprFunc(func(b *entsql.Builder) {
		b.WriteString("(COALESCE(" + lead.FieldMetadata + ", '{}'::jsonb) || ")
		b.Arg(string(patch))
		b.WriteString("::jsonb)")
	})
	_, err := db.Lead.Update().
		Where(lead.IDEQ(leadID), func(s *entsql.Selector) {
			s.Where(entsql.Not(entsql.ExprP("jsonb_exists(COALESCE(" + s.C(lead.FieldMetadata) + ", '{}'::jsonb), 'first_reply_at')")))
		}).
		Modify(func(u *entsql.UpdateBuilder) { u.Set(lead.FieldMetadata, value) }).
		Save(ctx)
	return err
}

// whatsAppConsent reports the lead's own WhatsApp consent: metadata.whatsapp_consent true, set by
// the consent tick on lead forms, funnels and the widget.
func whatsAppConsent(l *ent.Lead) bool {
	v, _ := l.Metadata["whatsapp_consent"].(bool)
	return v
}
