package worker

import (
	"context"
	"fmt"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/nurtureenrollment"
	"github.com/bengobox/marketflow-api/internal/ent/nurturesequence"
	"github.com/bengobox/marketflow-api/internal/modules/leads"
	"github.com/bengobox/marketflow-api/internal/platform/leadresponse"
)

// Nurture enrolment is keyed by lead and funnel (owner, 2026-11-24). One person is one lead, so a
// person who comes back through another funnel is not a new lead; that funnel can still enrol
// them, while the same funnel never enrols them twice. A sequence may name its funnel in
// metadata.funnel_id; such a sequence wins for leads from that funnel, and a sequence without one
// serves everyone else.

// enrolmentFunnelKey is the enrolment metadata key holding the funnel the enrolment came from
// ("" for a lead that came through no funnel).
const enrolmentFunnelKey = "funnel_key"

func funnelKey(ev leadCreated) string {
	if ev.FunnelID == uuid.Nil {
		return ""
	}
	return ev.FunnelID.String()
}

// pickSequence returns the sequence for this lead: one tied to its funnel first, else one tied to
// no funnel. Lead type filters apply to both.
func pickSequence(seqs []*ent.NurtureSequence, leadType, key string) *ent.NurtureSequence {
	typeOK := func(s *ent.NurtureSequence) bool {
		return s.LeadTypeFilter == nil || *s.LeadTypeFilter == "" || *s.LeadTypeFilter == leadType
	}
	seqFunnel := func(s *ent.NurtureSequence) string {
		v, _ := s.Metadata["funnel_id"].(string)
		return v
	}
	if key != "" {
		for _, s := range seqs {
			if typeOK(s) && seqFunnel(s) == key {
				return s
			}
		}
	}
	for _, s := range seqs {
		if typeOK(s) && seqFunnel(s) == "" {
			return s
		}
	}
	return nil
}

// enrolLead enrols a lead in the tenant's matching lead.created sequence. resubmitted is true for
// a known person's new submission: it enrols only when no enrolment exists for this lead and
// funnel yet.
func enrolLead(ctx context.Context, db *ent.Client, logger *zap.Logger, kick func(), ev leadCreated, resubmitted bool) error {
	// Only the tenant's own sequences. Platform templates (scope platform) live under the platform
	// tenant's id and are offered to every tenant to copy.
	seqs, err := db.NurtureSequence.Query().Where(
		nurturesequence.TenantIDEQ(ev.TenantID),
		nurturesequence.ScopeEQ("tenant"),
		nurturesequence.TriggerEventEQ("lead.created"),
		nurturesequence.ActiveEQ(true),
	).Order(ent.Asc(nurturesequence.FieldCreatedAt), ent.Asc(nurturesequence.FieldID)).All(ctx)
	if err != nil {
		return fmt.Errorf("query sequences: %w", err)
	}
	key := funnelKey(ev)
	chosen := pickSequence(seqs, ev.LeadType, key)
	if chosen == nil && len(seqs) == 0 {
		// A tenant without a follow-up of its own gets its copy of the default one now, so its
		// first lead is answered even before the hourly provisioner ran (owner, 2026-11-24).
		seq, err := leadresponse.ProvisionFollowUp(ctx, db, ev.TenantID)
		if err != nil {
			logger.Warn("default follow-up not provisioned", zap.String("tenant", ev.TenantID.String()), zap.Error(err))
		} else if seq != nil {
			chosen = pickSequence([]*ent.NurtureSequence{seq}, ev.LeadType, key)
		}
	}
	if chosen == nil {
		logger.Debug("no matching nurture sequence", zap.String("tenant", ev.TenantID.String()), zap.String("lead_type", ev.LeadType))
		return nil
	}
	if leadresponse.IsDefaultCopy(chosen) {
		if ok, err := defaultFollowUpApplies(ctx, db, ev.TenantID, ev.LeadID); err != nil || !ok {
			return err
		}
	}

	mine := db.NurtureEnrollment.Query().Where(nurtureenrollment.TenantIDEQ(ev.TenantID), nurtureenrollment.LeadIDEQ(ev.LeadID))
	// A redelivered lead.created must not enrol the lead twice in its sequence; a repeat
	// submission must not start a sequence the lead is still in.
	sameSeq := mine.Clone().Where(nurtureenrollment.SequenceIDEQ(chosen.ID))
	if resubmitted {
		sameSeq = sameSeq.Where(nurtureenrollment.StatusEQ("active"))
	}
	if already, err := sameSeq.Exist(ctx); err != nil || already {
		return err
	}
	if resubmitted {
		done, err := enrolledForFunnel(ctx, db, mine, ev, key)
		if err != nil || done {
			return err
		}
	}

	now := time.Now().UTC()
	trigger := leads.LeadCreatedTopic
	if resubmitted {
		trigger = leads.LeadResubmittedTopic
	}
	enrollment, err := db.NurtureEnrollment.Create().
		SetTenantID(ev.TenantID).
		SetSequenceID(chosen.ID).
		SetLeadID(ev.LeadID).
		SetCurrentStep(0).
		SetStatus("active").
		SetNextStepAt(now). // fire first step ASAP
		SetMetadata(map[string]any{"source": ev.Source, "auto_enrolled": true, enrolmentFunnelKey: key, "trigger": trigger}).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("create enrollment: %w", err)
	}
	// Bump sequence counter (best-effort; not critical if it fails).
	_, _ = db.NurtureSequence.UpdateOneID(chosen.ID).AddTotalEnrolled(1).Save(ctx)
	if kick != nil {
		kick()
	}
	logger.Info("lead auto-enrolled",
		zap.String("tenant", ev.TenantID.String()), zap.String("lead", ev.LeadID.String()),
		zap.String("sequence", chosen.ID.String()), zap.String("enrollment", enrollment.ID.String()),
		zap.Bool("resubmitted", resubmitted))
	return nil
}

// defaultFollowUpApplies reports whether the tenant's copy of the default follow-up may enrol
// this lead: the tenant has the follow-up switched on (its override, else the platform default)
// and the lead arrived after lead response went live, so existing leads are never messaged.
func defaultFollowUpApplies(ctx context.Context, db *ent.Client, tenantID, leadID uuid.UUID) (bool, error) {
	t, err := db.Tenant.Get(ctx, tenantID)
	if err != nil {
		return false, err
	}
	if s, _ := leadresponse.Load(ctx, db, t); !s.FollowUp {
		return false, nil
	}
	started, ok := leadresponse.StartedAt(ctx, db)
	if !ok {
		return false, nil
	}
	l, err := db.Lead.Get(ctx, leadID)
	if err != nil {
		return false, nil
	}
	return !l.CreatedAt.Before(started), nil
}

// enrolledForFunnel reports whether the lead already has an enrolment for this funnel. Enrolments
// made before funnel keys were recorded count as the lead's own first funnel.
func enrolledForFunnel(ctx context.Context, db *ent.Client, mine *ent.NurtureEnrollmentQuery, ev leadCreated, key string) (bool, error) {
	keyed, err := mine.Clone().Where(func(s *entsql.Selector) {
		s.Where(entsql.P(func(b *entsql.Builder) {
			b.WriteString("COALESCE(" + s.C(nurtureenrollment.FieldMetadata) + "->>'" + enrolmentFunnelKey + "', '') = ").Arg(key)
			b.WriteString(" AND jsonb_exists(COALESCE(" + s.C(nurtureenrollment.FieldMetadata) + ", '{}'::jsonb), '" + enrolmentFunnelKey + "')")
		}))
	}).Exist(ctx)
	if err != nil || keyed {
		return keyed, err
	}
	l, err := db.Lead.Get(ctx, ev.LeadID)
	if err != nil {
		return false, nil
	}
	origin := ""
	if l.FunnelID != nil {
		origin = l.FunnelID.String()
	}
	if origin != key {
		return false, nil
	}
	return mine.Clone().Where(func(s *entsql.Selector) {
		s.Where(entsql.ExprP("NOT jsonb_exists(COALESCE(" + s.C(nurtureenrollment.FieldMetadata) + ", '{}'::jsonb), '" + enrolmentFunnelKey + "')"))
	}).Exist(ctx)
}

// LeadResubmittedHandler enrols a known person who came back through a funnel that has not
// enrolled them yet (lead.resubmitted). lead.created consumers (alerts, conversions, scoring)
// see each person once.
type LeadResubmittedHandler struct {
	db     *ent.Client
	logger *zap.Logger
	kick   func()
}

func (h *LeadResubmittedHandler) Subject() string { return leads.LeadResubmittedTopic }
func (h *LeadResubmittedHandler) Durable() string { return "marketflow-worker-lead-resubmitted" }

func (h *LeadResubmittedHandler) Handle(ctx context.Context, msg jetstream.Msg) error {
	ev, err := decodeLeadResubmitted(msg.Data())
	if err != nil {
		return dropPoison(h.logger, msg, "decode lead.resubmitted", err)
	}
	if h.db == nil {
		return nil
	}
	return enrolLead(ctx, h.db, h.logger, h.kick, ev, true)
}
