package worker

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	sharedcache "github.com/Bengo-Hub/cache"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/activity"
	"github.com/bengobox/marketflow-api/internal/ent/deal"
	"github.com/bengobox/marketflow-api/internal/ent/scheduledmeeting"
	"github.com/bengobox/marketflow-api/internal/notifications"
)

// meetingPrepTimeout sits above marketflow-ai's own 20s cap on the briefing.
const meetingPrepTimeout = 30 * time.Second

// MeetingPrepAgent emails the organizer a short brief 25 to 35 minutes before
// each scheduled meeting: who it is with, when, the link and notes, their open
// deals and recent activity, plus AI talking points when marketflow-ai answers
// in time. Without them the brief still goes out (never-stall rule). Each
// meeting is sent once: a fleet-wide claim per meeting, then metadata.prep_sent.
type MeetingPrepAgent struct {
	db         *ent.Client
	notif      *notifications.Client
	aiBaseURL  string
	serviceKey string
	portalURL  string
	logger     *zap.Logger
	hc         *http.Client
	nowFn      func() time.Time
	claim      claimFn
	claimTick  func(ctx context.Context, name string, period time.Duration) bool
}

// NewMeetingPrepAgent constructs a MeetingPrepAgent.
func NewMeetingPrepAgent(db *ent.Client, notif *notifications.Client, aiBaseURL, serviceKey, portalURL string, logger *zap.Logger) *MeetingPrepAgent {
	return &MeetingPrepAgent{db: db, notif: notif, aiBaseURL: strings.TrimSuffix(aiBaseURL, "/"), serviceKey: serviceKey,
		portalURL: strings.TrimSuffix(portalURL, "/"), logger: logger, hc: &http.Client{Timeout: meetingPrepTimeout},
		nowFn: time.Now, claim: sharedcache.ClaimOnce, claimTick: sharedcache.ClaimPeriod}
}

// Run ticks every five minutes (a meeting's 10-minute window is seen at least
// once) until ctx ends.
func (a *MeetingPrepAgent) Run(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.run(ctx)
		}
	}
}

func (a *MeetingPrepAgent) run(ctx context.Context) {
	// Every replica ticks; only the first in each period scans.
	if !a.claimTick(ctx, "marketflow:meeting-prep", 5*time.Minute) {
		return
	}
	sent, err := a.scan(ctx)
	if err != nil {
		a.logger.Error("meeting prep scan failed", zap.Error(err))
	} else if sent > 0 {
		a.logger.Info("meeting prep briefs sent", zap.Int("meetings", sent))
	}
}

// scan sends the brief for scheduled meetings starting 25 to 35 minutes from now.
func (a *MeetingPrepAgent) scan(ctx context.Context) (int, error) {
	now := a.nowFn().UTC()
	meetings, err := a.db.ScheduledMeeting.Query().
		Where(
			scheduledmeeting.StatusEQ("scheduled"),
			scheduledmeeting.ScheduledAtGTE(now.Add(25*time.Minute)),
			scheduledmeeting.ScheduledAtLTE(now.Add(35*time.Minute)),
		).
		Limit(500). // a ten-minute window; bounds bad data
		All(ctx)
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, m := range meetings {
		if done, _ := m.Metadata["prep_sent"].(bool); done {
			continue
		}
		if !a.claim(ctx, "marketflow:meeting-prep:"+m.ID.String(), 48*time.Hour) {
			continue
		}
		if a.sendPrep(ctx, m) {
			sent++
		}
	}
	return sent, nil
}

// prepFacts is what the brief says, gathered from marketflow-api's own data.
type prepFacts struct {
	With       string
	When       string
	Link       string
	Notes      string
	OpenDeals  []string
	Recent     []string
	Talking    []string
	Background string
}

// sendPrep builds and emails one brief; it reports whether anyone got it.
func (a *MeetingPrepAgent) sendPrep(ctx context.Context, m *ent.ScheduledMeeting) bool {
	if a.notif == nil {
		return false
	}
	recipients := []string{}
	if m.OrganizerEmail != nil && strings.TrimSpace(*m.OrganizerEmail) != "" {
		recipients = append(recipients, strings.TrimSpace(*m.OrganizerEmail))
	} else if admins, err := tenantAdminEmails(ctx, a.db, m.TenantID, briefRoles); err == nil {
		recipients = admins
	}
	if len(recipients) == 0 {
		a.logger.Info("meeting prep: nobody to send to", zap.String("meeting_id", m.ID.String()))
		return false
	}

	f := a.facts(ctx, m)
	if m.ContactID != nil {
		f.Talking, f.Background = a.talkingPoints(ctx, m.TenantID, *m.ContactID, m.MeetingType)
	}
	subject := fmt.Sprintf("In 30 minutes: meeting with %s", f.With)
	body := renderMeetingPrep(f, a.portalURL)

	delivered := false
	for _, to := range recipients {
		err := a.notif.Send(ctx, m.TenantID.String(), notifications.Message{
			Channel:        "email",
			Recipient:      to,
			Subject:        subject,
			Body:           body,
			Metadata:       map[string]string{"source": "marketflow-worker", "kind": "meeting_prep"},
			IdempotencyKey: "meeting-prep-" + m.ID.String() + "-" + to,
		})
		if err != nil {
			a.logger.Warn("meeting prep not sent", zap.String("meeting_id", m.ID.String()), zap.Error(err))
			continue
		}
		delivered = true
	}
	if delivered {
		meta := map[string]any{}
		for k, v := range m.Metadata {
			meta[k] = v
		}
		meta["prep_sent"] = true
		meta["prep_sent_at"] = a.nowFn().UTC().Format(time.RFC3339)
		if _, err := a.db.ScheduledMeeting.UpdateOneID(m.ID).SetMetadata(meta).Save(ctx); err != nil {
			a.logger.Warn("meeting prep: could not mark sent", zap.String("meeting_id", m.ID.String()), zap.Error(err))
		}
	}
	return delivered
}

// facts reads the meeting, the contact's open deals and the last activities.
// Every lookup is optional: a missing piece leaves a shorter brief.
func (a *MeetingPrepAgent) facts(ctx context.Context, m *ent.ScheduledMeeting) prepFacts {
	f := prepFacts{With: "your contact"}
	if m.AttendeeName != nil && strings.TrimSpace(*m.AttendeeName) != "" {
		f.With = strings.TrimSpace(*m.AttendeeName)
	} else if m.AttendeeEmail != nil && *m.AttendeeEmail != "" {
		f.With = *m.AttendeeEmail
	}
	loc := a.zone(ctx, m.TenantID)
	f.When = m.ScheduledAt.In(loc).Format("Mon 2 Jan, 15:04") + fmt.Sprintf(" (%d minutes)", m.DurationMinutes)
	if m.MeetingURL != nil {
		f.Link = *m.MeetingURL
	}
	if m.Notes != nil {
		f.Notes = truncateStr(strings.TrimSpace(*m.Notes), 500)
	}

	var entities []uuid.UUID
	if m.ContactID != nil {
		entities = append(entities, *m.ContactID)
		if deals, err := a.db.Deal.Query().
			Where(deal.TenantID(m.TenantID), deal.ContactID(*m.ContactID), deal.StatusEQ("open")).
			Order(ent.Desc(deal.FieldUpdatedAt)).Limit(3).All(ctx); err == nil {
			for _, d := range deals {
				line := d.Title
				if d.Value != nil {
					line += fmt.Sprintf(", %s %.0f", d.Currency, *d.Value)
				}
				f.OpenDeals = append(f.OpenDeals, line)
			}
		}
	}
	if m.LeadID != nil {
		entities = append(entities, *m.LeadID)
	}
	if len(entities) > 0 {
		if acts, err := a.db.Activity.Query().
			Where(activity.TenantID(m.TenantID), activity.EntityIDIn(entities...)).
			Order(ent.Desc(activity.FieldPerformedAt)).Limit(3).All(ctx); err == nil {
			for _, act := range acts {
				line := act.PerformedAt.In(loc).Format("2 Jan") + ": " + act.ActivityType
				if act.Subject != nil && *act.Subject != "" {
					line += ", " + truncateStr(*act.Subject, 120)
				}
				f.Recent = append(f.Recent, line)
			}
		}
	}
	return f
}

// talkingPoints asks marketflow-ai for a short background and talking points.
// It returns nothing on any failure; the brief goes out without them.
func (a *MeetingPrepAgent) talkingPoints(ctx context.Context, tenantID, contactID uuid.UUID, purpose string) ([]string, string) {
	if a.aiBaseURL == "" || a.serviceKey == "" {
		return nil, ""
	}
	var out struct {
		BioSummary    string   `json:"bio_summary"`
		TalkingPoints []string `json:"talking_points"`
	}
	cctx, cancel := context.WithTimeout(ctx, meetingPrepTimeout)
	defer cancel()
	err := agentS2SCall(cctx, a.hc, a.aiBaseURL+"/api/v1/internal/os/meeting-prep", a.serviceKey,
		map[string]any{"tenant_id": tenantID.String(), "contact_id": contactID.String(), "purpose": purpose}, &out)
	if err != nil {
		a.logger.Info("meeting prep: no talking points, sending the brief without them", zap.Error(err))
		return nil, ""
	}
	points := make([]string, 0, 3)
	for _, p := range out.TalkingPoints {
		if p = strings.TrimSpace(p); p != "" && len(points) < 3 {
			points = append(points, truncateStr(p, 240))
		}
	}
	return points, truncateStr(strings.TrimSpace(out.BioSummary), 400)
}

func (a *MeetingPrepAgent) zone(ctx context.Context, tenantID uuid.UUID) *time.Location {
	if t, err := a.db.Tenant.Get(ctx, tenantID); err == nil && t.Timezone != nil && *t.Timezone != "" {
		if loc, err := time.LoadLocation(*t.Timezone); err == nil {
			return loc
		}
	}
	if loc, err := time.LoadLocation("Africa/Nairobi"); err == nil {
		return loc
	}
	return time.FixedZone("EAT", 3*3600)
}

// renderMeetingPrep writes the brief as plain-text paragraphs separated by
// blank lines, which notifications-api renders as email paragraphs.
func renderMeetingPrep(f prepFacts, portalURL string) string {
	var parts []string
	head := "Meeting with " + f.With + ", " + f.When + "."
	if f.Link != "" {
		head += "\nJoin: " + f.Link
	}
	parts = append(parts, head)
	if f.Background != "" {
		parts = append(parts, f.Background)
	}
	if f.Notes != "" {
		parts = append(parts, "Their note when booking: "+f.Notes)
	}
	if len(f.OpenDeals) > 0 {
		parts = append(parts, "Open deals:\n- "+strings.Join(f.OpenDeals, "\n- "))
	}
	if len(f.Recent) > 0 {
		parts = append(parts, "Recent activity:\n- "+strings.Join(f.Recent, "\n- "))
	}
	if len(f.Talking) > 0 {
		parts = append(parts, "Talking points:\n- "+strings.Join(f.Talking, "\n- "))
	}
	if portalURL != "" {
		parts = append(parts, "Your meetings: "+portalURL)
	}
	return strings.Join(parts, "\n\n")
}
