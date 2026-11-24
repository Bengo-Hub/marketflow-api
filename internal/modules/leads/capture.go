package leads

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"

	entsql "entgo.io/ent/dialect/sql"
	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/outbox"
)

// One person is one lead per tenant (owner, 2026-11-24). A submission whose email or phone the
// tenant already has goes on that lead as an activity, with its own source, funnel and click ids,
// instead of making a second lead. Email compares case-insensitively; phone compares by its last
// nine digits, the same subscriber key the contacts dedup uses, so 0712345678, +254712345678 and
// 254 712 345 678 are one number.

// LeadResubmittedEvent goes out when a known person submits again. lead.created goes out only
// for a new person.
const (
	LeadResubmittedEvent = "resubmitted"
	LeadResubmittedTopic = LeadAggregate + "." + LeadResubmittedEvent
)

// ActivitySubmission is the activity type of a repeat submission on a lead.
const ActivitySubmission = "form_submission"

// LeadResubmittedPayload is the payload of lead.resubmitted. Like lead.created it carries no
// personal data.
type LeadResubmittedPayload struct {
	LeadID     string `json:"lead_id"`
	LeadType   string `json:"lead_type"`
	Source     string `json:"source"`
	CampaignID string `json:"campaign_id,omitempty"`
	FunnelID   string `json:"funnel_id,omitempty"`
	ActivityID string `json:"activity_id"`
}

// EmailKey is the comparison form of an email: trimmed and lower case.
func EmailKey(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// PhoneKey is the comparison form of a phone number: its last nine digits, or "" when it has
// fewer than seven digits (too short to be a number).
func PhoneKey(phone string) string {
	var b strings.Builder
	for _, r := range phone {
		if unicode.IsDigit(r) && r < 128 {
			b.WriteRune(r)
		}
	}
	d := b.String()
	if len(d) < 7 {
		return ""
	}
	if len(d) > 9 {
		d = d[len(d)-9:]
	}
	return d
}

// matchKeys are the keys a lead input is matched on.
func matchKeys(email, phone *string) (string, string) {
	e, p := "", ""
	if email != nil {
		e = EmailKey(*email)
	}
	if phone != nil {
		p = PhoneKey(*phone)
	}
	return e, p
}

// lockKeys takes a transaction-scoped advisory lock per key, so two submissions of the same new
// person at the same moment make one lead: the second waits, then finds the first.
func lockKeys(ctx context.Context, tx *ent.Tx, tenantID uuid.UUID, email, phone string) error {
	for _, k := range []string{"e:" + email, "p:" + phone} {
		if len(k) == 2 {
			continue
		}
		if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", "lead:"+tenantID.String()+":"+k); err != nil {
			return fmt.Errorf("lock lead key: %w", err)
		}
	}
	return nil
}

// findExisting returns the tenant's oldest lead with this email or phone, or nil.
func findExisting(ctx context.Context, q *ent.LeadClient, tenantID uuid.UUID, email, phone string) (*ent.Lead, error) {
	if email == "" && phone == "" {
		return nil, nil
	}
	found, err := q.Query().
		Where(lead.TenantID(tenantID), func(s *entsql.Selector) {
			var ps []*entsql.Predicate
			if email != "" {
				ps = append(ps, entsql.P(func(b *entsql.Builder) {
					b.WriteString("LOWER(TRIM(" + s.C(lead.FieldEmail) + ")) = ").Arg(email)
				}))
			}
			if phone != "" {
				ps = append(ps, entsql.P(func(b *entsql.Builder) {
					b.WriteString("RIGHT(REGEXP_REPLACE(" + s.C(lead.FieldPhone) + ", '[^0-9]', '', 'g'), 9) = ").Arg(phone)
				}))
			}
			s.Where(entsql.Or(ps...))
		}).
		Order(ent.Asc(lead.FieldCreatedAt), ent.Asc(lead.FieldID)).
		First(ctx)
	if ent.IsNotFound(err) {
		return nil, nil
	}
	return found, err
}

// CaptureResult is what Capture did: a new lead, or a submission recorded on a known one.
type CaptureResult struct {
	Lead       *ent.Lead
	Created    bool
	ActivityID uuid.UUID
}

// Capture saves a lead submission. A new person becomes a lead (lead.created); a person the
// tenant already has gets the submission as an activity on their lead, the lead's empty fields
// filled from it, a lost lead reopened, and lead.resubmitted. Everything commits in one
// transaction with its event.
func (s *Service) Capture(ctx context.Context, in CreateInput) (CaptureResult, error) {
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return CaptureResult{}, fmt.Errorf("capture lead: begin tx: %w", err)
	}
	fail := func(cause error) (CaptureResult, error) {
		_ = tx.Rollback()
		return CaptureResult{}, cause
	}
	// A fixed id that already exists is a redelivered webhook, never a new submission.
	if in.ID != nil {
		if ok, err := tx.Lead.Query().Where(lead.ID(*in.ID)).Exist(ctx); err != nil {
			return fail(err)
		} else if ok {
			return fail(ErrDuplicateLead)
		}
	}
	email, phone := matchKeys(in.Email, in.Phone)
	if err := lockKeys(ctx, tx, in.TenantID, email, phone); err != nil {
		return fail(err)
	}
	existing, err := findExisting(ctx, tx.Lead, in.TenantID, email, phone)
	if err != nil {
		return fail(fmt.Errorf("capture lead: match: %w", err))
	}
	if existing == nil {
		l, err := s.insert(ctx, tx, in)
		if err != nil {
			return fail(err)
		}
		if err := outbox.Enqueue(ctx, tx.OutboxEvent, leadCreatedEvent(l)); err != nil {
			return fail(err)
		}
		if err := tx.Commit(); err != nil {
			return CaptureResult{}, fmt.Errorf("capture lead: commit: %w", err)
		}
		s.logger.Info("lead created", zap.String("id", l.ID.String()), zap.String("tenant_id", in.TenantID.String()), zap.String("source", in.Source))
		return CaptureResult{Lead: l, Created: true}, nil
	}

	now := time.Now().UTC()
	l, err := fillFrom(ctx, tx, existing, in, now, true)
	if err != nil {
		return fail(err)
	}
	act, err := tx.Activity.Create().
		SetTenantID(in.TenantID).
		SetEntityType("lead").
		SetEntityID(l.ID).
		SetActivityType(ActivitySubmission).
		SetDirection("inbound").
		SetSubject("Submitted again via " + strings.ReplaceAll(in.Source, "_", " ")).
		SetNillableBody(in.Interest).
		SetSource("system").
		SetPerformedAt(now).
		SetMetadata(submissionMetadata(in)).
		Save(ctx)
	if err != nil {
		return fail(fmt.Errorf("capture lead: activity: %w", err))
	}
	if err := outbox.Enqueue(ctx, tx.OutboxEvent, leadResubmittedEvent(l, in, act.ID)); err != nil {
		return fail(err)
	}
	if err := tx.Commit(); err != nil {
		return CaptureResult{}, fmt.Errorf("capture lead: commit: %w", err)
	}
	s.logger.Info("lead submitted again", zap.String("id", l.ID.String()), zap.String("tenant_id", in.TenantID.String()), zap.String("source", in.Source))
	return CaptureResult{Lead: l, ActivityID: act.ID}, nil
}

// insert writes a new lead row from the input (no event).
func (s *Service) insert(ctx context.Context, tx *ent.Tx, in CreateInput) (*ent.Lead, error) {
	builder := tx.Lead.Create().
		SetTenantID(in.TenantID).
		SetSource(in.Source).
		SetLeadType(in.LeadType).
		SetNillableEmail(in.Email).
		SetNillablePhone(in.Phone).
		SetNillableFirstName(in.FirstName).
		SetNillableLastName(in.LastName).
		SetNillableCampaignID(in.CampaignID).
		SetNillableFunnelID(in.FunnelID).
		SetNillableUtmSource(in.UTMSource).
		SetNillableUtmMedium(in.UTMMedium).
		SetNillableUtmCampaign(in.UTMCampaign).
		SetNillableInterest(in.Interest)
	if in.ID != nil {
		builder.SetID(*in.ID)
	}
	if in.Status != "" {
		builder.SetStatus(in.Status)
	}
	if in.CreatedAt != nil {
		builder.SetCreatedAt(*in.CreatedAt).SetUpdatedAt(*in.CreatedAt)
	}
	if in.QualificationAnswers != nil {
		builder.SetQualificationAnswers(in.QualificationAnswers)
	}
	if in.Metadata != nil {
		builder.SetMetadata(in.Metadata)
	}
	l, err := builder.Save(ctx)
	if err != nil {
		if in.ID != nil && ent.IsConstraintError(err) {
			return nil, ErrDuplicateLead
		}
		return nil, fmt.Errorf("create lead: %w", err)
	}
	return l, nil
}

// fillFrom fills the known lead's empty fields from a new submission. Nothing the lead already
// has is overwritten: the first source, funnel and UTM stay as the lead's origin, and the new
// ones live on the submission activity. A WhatsApp consent given now is recorded; a lost lead
// that comes back is new again.
func fillFrom(ctx context.Context, tx *ent.Tx, l *ent.Lead, in CreateInput, now time.Time, reopen bool) (*ent.Lead, error) {
	u := tx.Lead.UpdateOne(l)
	empty := func(p *string) bool { return p == nil || strings.TrimSpace(*p) == "" }
	if empty(l.Email) && !empty(in.Email) {
		u.SetEmail(*in.Email)
	}
	if empty(l.Phone) && !empty(in.Phone) {
		u.SetPhone(*in.Phone)
	}
	if empty(l.FirstName) && !empty(in.FirstName) {
		u.SetFirstName(*in.FirstName)
	}
	if empty(l.LastName) && !empty(in.LastName) {
		u.SetLastName(*in.LastName)
	}
	if empty(l.Interest) && !empty(in.Interest) {
		u.SetInterest(*in.Interest)
	}
	if l.FunnelID == nil && in.FunnelID != nil {
		u.SetFunnelID(*in.FunnelID)
	}
	if l.CampaignID == nil && in.CampaignID != nil {
		u.SetCampaignID(*in.CampaignID)
	}
	meta := map[string]any{}
	for k, v := range l.Metadata {
		meta[k] = v
	}
	n, _ := meta["submission_count"].(float64)
	if n == 0 {
		n = 1
	}
	meta["submission_count"] = n + 1
	meta["last_submitted_at"] = now.Format(time.RFC3339)
	if c, _ := in.Metadata["whatsapp_consent"].(bool); c {
		meta["whatsapp_consent"] = true
		meta["whatsapp_consent_at"] = in.Metadata["whatsapp_consent_at"]
	}
	u.SetMetadata(meta)
	if reopen && l.Status == "lost" {
		u.SetStatus("new")
	}
	return u.Save(ctx)
}

// submissionMetadata is what a repeat submission carries for attribution: its source, funnel,
// campaign, UTM set, click ids and form answers.
func submissionMetadata(in CreateInput) map[string]any {
	out := map[string]any{"source": in.Source, "lead_type": in.LeadType}
	if in.FunnelID != nil {
		out["funnel_id"] = in.FunnelID.String()
	}
	if in.CampaignID != nil {
		out["campaign_id"] = in.CampaignID.String()
	}
	for k, v := range map[string]*string{"utm_source": in.UTMSource, "utm_medium": in.UTMMedium, "utm_campaign": in.UTMCampaign,
		"email": in.Email, "phone": in.Phone, "first_name": in.FirstName, "last_name": in.LastName} {
		if v != nil && *v != "" {
			out[k] = *v
		}
	}
	for k, v := range in.Metadata {
		out[k] = v
	}
	if in.QualificationAnswers != nil {
		out["answers"] = in.QualificationAnswers
	}
	return out
}

func leadResubmittedEvent(l *ent.Lead, in CreateInput, activityID uuid.UUID) *eventslib.Event {
	p := map[string]any{
		"lead_id":     l.ID.String(),
		"lead_type":   l.LeadType,
		"source":      in.Source,
		"activity_id": activityID.String(),
	}
	if in.CampaignID != nil {
		p["campaign_id"] = in.CampaignID.String()
	}
	if in.FunnelID != nil {
		p["funnel_id"] = in.FunnelID.String()
	}
	return eventslib.NewEvent(LeadResubmittedEvent, LeadAggregate, l.ID, l.TenantID, p)
}
