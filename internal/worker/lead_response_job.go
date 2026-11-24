package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	sharedcache "github.com/Bengo-Hub/cache"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/bengobox/marketflow-api/internal/modules/rbac"
	"github.com/bengobox/marketflow-api/internal/notifications"
	"github.com/bengobox/marketflow-api/internal/platform/leadresponse"
)

// LeadResponse is the lead response job (owner, 2026-11-24). Every tick, on one replica, it walks
// the tenants and for each one:
//   - sends the leads still waiting for a team alert (NewLeadAlertHandler.Flush);
//   - reminds the tenant's admins by email about leads nobody has contacted, at each reminder step
//     (15 minutes, 2 hours, 24 hours of business time by default), each step once per lead.
//
// Every hour it also gives tenants without a follow-up their copy of the default one
// (leadresponse.ProvisionFollowUp). Only leads created after lead response went live count.
type LeadResponse struct {
	db        *ent.Client
	notif     *notifications.Client
	alerts    *NewLeadAlertHandler
	appURL    string
	every     time.Duration
	provision time.Duration
	logger    *zap.Logger
	nowFn     func() time.Time
	claim     func(ctx context.Context, name string, period time.Duration) bool
}

const (
	// slaRemindedKey in lead metadata is the number of reminder steps already sent for the lead.
	slaRemindedKey = "sla_reminder_step"
	// slaLookback bounds the leads a reminder pass reads: past the last reminder of any sane
	// schedule (7 days of business time can span about two weeks of calendar time).
	slaLookback = 21 * 24 * time.Hour
	// slaBatch bounds the leads one tenant's pass reads.
	slaBatch = 500
	// tenantPage is how many tenants one query reads.
	tenantPage = 200
)

// NewLeadResponse builds the job: every 2 minutes, provisioning hourly.
func NewLeadResponse(db *ent.Client, notif *notifications.Client, alerts *NewLeadAlertHandler, appURL string, logger *zap.Logger) *LeadResponse {
	return &LeadResponse{db: db, notif: notif, alerts: alerts, appURL: strings.TrimRight(appURL, "/"),
		every: 2 * time.Minute, provision: time.Hour, logger: logger.Named("lead-response"), nowFn: time.Now,
		claim: sharedcache.ClaimPeriod}
}

// Run records the start time, provisions at once (the backfill on the first release) and then
// ticks until ctx ends.
func (j *LeadResponse) Run(ctx context.Context) {
	if _, err := leadresponse.EnsureStartedAt(ctx, j.db, j.nowFn()); err != nil {
		j.logger.Warn("lead response start time not recorded", zap.Error(err))
	}
	t := time.NewTicker(j.every)
	defer t.Stop()
	for {
		if j.claim(ctx, "marketflow:lead-followup-provision", j.provision) {
			if n, err := j.ProvisionAll(ctx); err != nil {
				j.logger.Warn("follow-up provisioning failed", zap.Int("made_before_error", n), zap.Error(err))
			} else if n > 0 {
				j.logger.Info("default follow-ups provisioned", zap.Int("tenants", n))
			}
		}
		if j.claim(ctx, "marketflow:lead-response", j.every) {
			if err := j.tick(ctx); err != nil {
				j.logger.Warn("lead response pass failed", zap.Error(err))
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// forEachTenant pages through every tenant in id order.
func (j *LeadResponse) forEachTenant(ctx context.Context, fn func(*ent.Tenant) error) error {
	after := uuid.Nil
	for {
		q := j.db.Tenant.Query().Order(ent.Asc(tenant.FieldID)).Limit(tenantPage)
		if after != uuid.Nil {
			q = q.Where(tenant.IDGT(after))
		}
		rows, err := q.All(ctx)
		if err != nil {
			return err
		}
		for _, t := range rows {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err := fn(t); err != nil {
				j.logger.Warn("lead response tenant failed", zap.String("tenant", t.ID.String()), zap.Error(err))
			}
		}
		if len(rows) < tenantPage {
			return nil
		}
		after = rows[len(rows)-1].ID
	}
}

// ProvisionAll gives every tenant that has none its copy of the default follow-up.
func (j *LeadResponse) ProvisionAll(ctx context.Context) (int, error) {
	made := 0
	err := j.forEachTenant(ctx, func(t *ent.Tenant) error {
		seq, err := leadresponse.ProvisionFollowUp(ctx, j.db, t.ID)
		if seq != nil {
			made++
		}
		return err
	})
	return made, err
}

func (j *LeadResponse) tick(ctx context.Context) error {
	started, ok := leadresponse.StartedAt(ctx, j.db)
	if !ok {
		return nil
	}
	platform := leadresponse.Platform(ctx, j.db)
	return j.forEachTenant(ctx, func(t *ent.Tenant) error {
		if _, err := j.alerts.Flush(ctx, t); err != nil {
			j.logger.Warn("lead alert flush", zap.String("tenant", t.ID.String()), zap.Error(err))
		}
		_, err := j.remind(ctx, t, platform, started)
		return err
	})
}

// reminderDue is one lead's reminder: the step reached (1-based) and its offset.
type reminderDue struct {
	lead  *ent.Lead
	step  int
	after time.Duration
}

// remind emails the tenant's admins about its uncontacted leads that reached a new reminder step,
// all in one email, and returns how many leads it named. A lead that reached several steps at
// once (the worker was down) is named once, at the latest step.
func (j *LeadResponse) remind(ctx context.Context, t *ent.Tenant, platform leadresponse.Settings, started time.Time) (int, error) {
	s := leadresponse.Merge(platform, t.Metadata, leadresponse.TenantLocation(t))
	if len(s.Reminders) == 0 {
		return 0, nil
	}
	now := j.nowFn()
	since := now.Add(-slaLookback)
	if started.After(since) {
		since = started
	}
	// Served by leads(tenant_id, status); the reminder step is checked in the claim below.
	rows, err := j.db.Lead.Query().
		Where(lead.TenantID(t.ID), lead.StatusEQ(leadresponse.StatusNew), lead.CreatedAtGTE(since),
			// The first step falls due at the earliest after its business-time offset.
			lead.CreatedAtLTE(now.Add(-s.Reminders[0]))).
		Order(ent.Asc(lead.FieldCreatedAt)).
		Limit(slaBatch).
		All(ctx)
	if err != nil {
		return 0, err
	}
	var due []reminderDue
	for _, l := range rows {
		reached := 0
		for i, r := range s.Reminders {
			if !now.Before(s.AddBusiness(l.CreatedAt, r)) {
				reached = i + 1
			}
		}
		if reached == 0 || reached <= remindedStep(l) {
			continue
		}
		ok, err := claimReminderStep(ctx, j.db, l.ID, reached, now)
		if err != nil {
			j.logger.Warn("reminder claim", zap.String("lead", l.ID.String()), zap.Error(err))
			continue
		}
		if ok {
			due = append(due, reminderDue{lead: l, step: reached, after: s.Reminders[reached-1]})
		}
	}
	if len(due) == 0 || j.notif == nil {
		return 0, nil
	}
	emails, err := rbac.TenantAdminEmails(ctx, j.db, t.ID, rbac.NoticeRoles)
	if err != nil || len(emails) == 0 {
		return 0, err
	}
	link := j.appURL + "/" + t.Slug + "/leads?status=new"
	subject, body := reminderText(due, len(s.Reminders), func(id uuid.UUID) string {
		return j.appURL + "/" + t.Slug + "/leads/" + id.String()
	}, link)
	keyParts := make([]string, len(due))
	for i, d := range due {
		keyParts[i] = fmt.Sprintf("%s:%d", d.lead.ID, d.step)
	}
	sum := sha256.Sum256([]byte(strings.Join(keyParts, ",")))
	for _, email := range emails {
		if err := j.notif.Send(ctx, t.ID.String(), notifications.Message{
			Channel: "email", Recipient: email, Subject: subject, Body: body,
			Metadata:       map[string]string{"source": "marketflow", "kind": "lead_reminder"},
			IdempotencyKey: "lead-reminder-" + hex.EncodeToString(sum[:8]) + "-" + email,
		}); err != nil {
			j.logger.Warn("lead reminder not sent", zap.String("tenant", t.ID.String()), zap.Error(err))
		}
	}
	return len(due), nil
}

// remindedStep is how many reminder steps were already sent for the lead.
func remindedStep(l *ent.Lead) int {
	n, _ := l.Metadata[slaRemindedKey].(float64)
	return int(n)
}

// claimReminderStep records that reminder step `step` went out for a lead still new and not yet
// reminded at that step, in one conditional UPDATE, so each step goes once across replicas.
func claimReminderStep(ctx context.Context, db *ent.Client, leadID uuid.UUID, step int, at time.Time) (bool, error) {
	patch, _ := json.Marshal(map[string]any{slaRemindedKey: step, "sla_reminded_at": at.UTC().Format(time.RFC3339)})
	expr := entsql.ExprFunc(func(b *entsql.Builder) {
		b.WriteString("(COALESCE(" + lead.FieldMetadata + ", '{}'::jsonb) || ")
		b.Arg(string(patch))
		b.WriteString("::jsonb)")
	})
	n, err := db.Lead.Update().
		Where(lead.IDEQ(leadID), lead.StatusEQ(leadresponse.StatusNew), func(s *entsql.Selector) {
			s.Where(entsql.P(func(b *entsql.Builder) {
				b.WriteString("COALESCE((" + s.C(lead.FieldMetadata) + "->>'" + slaRemindedKey + "')::int, 0) < ").Arg(step)
			}))
		}).
		Modify(func(u *entsql.UpdateBuilder) { u.Set(lead.FieldMetadata, expr) }).
		Save(ctx)
	return n > 0, err
}

// reminderText is the reminder email: every lead waiting, how long, and where to answer them.
func reminderText(due []reminderDue, steps int, leadLink func(uuid.UUID) string, listLink string) (string, string) {
	first := leadDisplayName(due[0].lead)
	subject := fmt.Sprintf("%s is still waiting for a reply", first)
	intro := fmt.Sprintf("%s has not been contacted yet, %s of business hours after getting in touch.", first, humanDuration(due[0].after))
	if len(due) > 1 {
		subject = fmt.Sprintf("%d leads are still waiting for a reply", len(due))
		intro = fmt.Sprintf("%d leads have not been contacted yet. A quick reply is the best way not to lose them.", len(due))
	}
	parts := []string{intro}
	for _, d := range due {
		lines := []string{fmt.Sprintf("%s, waiting %s of business hours", leadDisplayName(d.lead), humanDuration(d.after))}
		if it := strings.TrimSpace(deref(d.lead.Interest)); it != "" {
			lines = append(lines, "Interested in: "+it)
		}
		if p := strings.TrimSpace(deref(d.lead.Phone)); p != "" {
			lines = append(lines, "Phone: "+p)
		}
		if e := strings.TrimSpace(deref(d.lead.Email)); e != "" {
			lines = append(lines, "Email: "+e)
		}
		if d.step == steps {
			lines = append(lines, "This is the last reminder for this lead; it stays on the daily list until someone contacts them.")
		}
		lines = append(lines, "Open the lead: "+leadLink(d.lead.ID))
		parts = append(parts, strings.Join(lines, "\n"))
	}
	parts = append(parts, "All leads still waiting: "+listLink,
		"A lead stops counting as waiting once someone replies from the inbox, logs a call or email, books a meeting with them or changes its status.")
	return subject, strings.Join(parts, "\n\n")
}
