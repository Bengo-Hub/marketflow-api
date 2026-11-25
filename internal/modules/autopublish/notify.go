package autopublish

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/modules/custom_agents"
	"github.com/bengobox/marketflow-api/internal/modules/publishing"
	"github.com/bengobox/marketflow-api/internal/modules/rbac"
	"github.com/bengobox/marketflow-api/internal/notifications"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// The heads-up and the fallback notices go to the tenant's admins by email through
// notifications-api, the same path as the autopilot's draft and credits notices.

// eastAfrica is used when the tenant's own time zone cannot be loaded.
var eastAfrica = func() *time.Location {
	if l, err := time.LoadLocation("Africa/Nairobi"); err == nil {
		return l
	}
	return time.FixedZone("EAT", 3*60*60)
}()

// tenantZone is the tenant's time zone for the times in an email.
func tenantZone(t *ent.Tenant) *time.Location {
	if t != nil && t.Timezone != nil && *t.Timezone != "" {
		if l, err := time.LoadLocation(*t.Timezone); err == nil {
			return l
		}
	}
	return eastAfrica
}

// portalLink is a link into the tenant's portal.
func (p *Pipeline) portalLink(t *ent.Tenant, path string) string {
	if t != nil && t.Slug != "" {
		return p.AppURL + "/" + t.Slug + path
	}
	return p.AppURL + path
}

// VetoURL is the emailed cancel link: a public portal page that shows the post and asks before it
// cancels, so a mail scanner opening the link never cancels anything.
func (p *Pipeline) VetoURL(token string) string {
	return strings.TrimSuffix(p.AppURL, "/") + "/veto/" + token
}

// HeadsUp is the scheduled post's notice, built apart from sending so it is tested on its own.
type HeadsUp struct {
	Subject string
	Body    string
}

// BuildHeadsUp writes the heads-up: what will be posted, where, when, and the cancel link.
func BuildHeadsUp(agentName, offering, caption string, where []string, at time.Time, zone *time.Location, studioLink, cancelLink string, paid bool) HeadsUp {
	when := at.In(zone).Format("Monday 2 January at 15:04")
	var b strings.Builder
	fmt.Fprintf(&b, "Your agent \"%s\" finished a video ad for %s. It will be posted to %s on %s (%s), unless you cancel it before then.\n\n",
		agentName, offering, strings.Join(where, ", "), when, zone.String())
	if caption != "" {
		fmt.Fprintf(&b, "The caption:\n%s\n\n", caption)
	}
	fmt.Fprintf(&b, "Watch it in your studio: %s\n\n", studioLink)
	fmt.Fprintf(&b, "Cancel this post: %s\n\n", cancelLink)
	if paid {
		b.WriteString("This is an organic post. The agent never starts a paid boost: a boost waits for your approval and a funded ad account.\n\n")
	}
	b.WriteString("If you do nothing, the post goes out at that time and the video counts as approved because the veto window passed.")
	return HeadsUp{Subject: fmt.Sprintf("Your agent will post an ad on %s unless you cancel it", at.In(zone).Format("2 Jan at 15:04")), Body: b.String()}
}

// headsUpRecipients are the admins the heads-up goes to; none means no post is scheduled.
func (p *Pipeline) headsUpRecipients(ctx context.Context, tenantID uuid.UUID) []string {
	emails, err := rbac.TenantAdminEmails(ctx, p.DB, tenantID, rbac.NoticeRoles)
	if err != nil {
		p.logger.Warn("autopilot heads-up recipients not read", zap.String("tenant", tenantID.String()), zap.Error(err))
		return nil
	}
	return emails
}

// tellScheduled sends the heads-up to the tenant's admins.
func (p *Pipeline) tellScheduled(ctx context.Context, v *ent.CreativeVersion, brief *ent.CreativeBrief, agent *ent.CustomAgent,
	cfg custom_agents.AutopilotConfig, out publishing.VetoSchedule, at time.Time, emails []string) {
	if p.Notif == nil || p.Links == nil {
		return
	}
	t, _ := p.DB.Tenant.Get(ctx, v.TenantID)
	token := p.Links.Sign(v.ID, at.Add(p.Config.VetoLinkGrace))
	hu := BuildHeadsUp(agent.Name, offeringName(brief), captionOf(out.Jobs, Caption(v, brief)), placeNames(ctx, p.DB, out.Jobs), at, tenantZone(t),
		p.portalLink(t, "/studio/"+v.BriefID.String()), p.VetoURL(token), cfg.Paid)
	for _, email := range emails {
		if err := p.Notif.Send(ctx, v.TenantID.String(), notifications.Message{
			Channel: notifications.ChannelEmail, Recipient: email, Subject: hu.Subject, Body: hu.Body,
			Metadata:       map[string]string{"source": "marketflow", "kind": "autopilot_veto_window"},
			IdempotencyKey: "autopilot-veto-" + v.ID.String() + "-" + email,
		}); err != nil {
			p.logger.Warn("autopilot heads-up not sent", zap.String("version_id", v.ID.String()), zap.Error(err))
		}
	}
}

// tellCancelled tells the admins which waiting posts were cancelled because the agent was turned
// off, and which (if any) had already reached the platform.
func (p *Pipeline) tellCancelled(ctx context.Context, v *ent.CreativeVersion, brief *ent.CreativeBrief, why string, out publishing.VetoOutcome) {
	if p.Notif == nil {
		return
	}
	emails, err := rbac.TenantAdminEmails(ctx, p.DB, v.TenantID, rbac.NoticeRoles)
	if err != nil || len(emails) == 0 {
		return
	}
	t, _ := p.DB.Tenant.Get(ctx, v.TenantID)
	body := BuildAgentOffNotice(offeringName(brief), why, out.Cancelled, placeNames(ctx, p.DB, out.Gone),
		p.portalLink(t, "/studio/"+v.BriefID.String()))
	for _, email := range emails {
		if err := p.Notif.Send(ctx, v.TenantID.String(), notifications.Message{
			Channel: notifications.ChannelEmail, Recipient: email, Subject: "A scheduled ad post was cancelled", Body: body,
			Metadata:       map[string]string{"source": "marketflow", "kind": "autopilot_agent_off"},
			IdempotencyKey: "autopilot-agent-off-" + v.ID.String() + "-" + email,
		}); err != nil {
			p.logger.Warn("autopilot cancel notice not sent", zap.String("version_id", v.ID.String()), zap.Error(err))
		}
	}
}

// BuildAgentOffNotice writes the email about posts cancelled because the agent was turned off.
func BuildAgentOffNotice(offering, why string, cancelled int, gone []string, studioLink string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d scheduled post(s) of the video ad for %s were cancelled because %s. Nothing more of it will be posted by the agent.\n\n",
		cancelled, offering, why)
	if len(gone) > 0 {
		fmt.Fprintf(&b, "Already on its way before the cancel: %s.\n\n", strings.Join(gone, ", "))
	}
	fmt.Fprintf(&b, "The video stays in your studio, where you can approve and post it yourself: %s", studioLink)
	return b.String()
}

// tellHeld tells the admins a video fell back to approve-each, and why.
func (p *Pipeline) tellHeld(ctx context.Context, v *ent.CreativeVersion, brief *ent.CreativeBrief, agent *ent.CustomAgent, reason string) {
	if p.Notif == nil {
		return
	}
	emails, err := rbac.TenantAdminEmails(ctx, p.DB, v.TenantID, rbac.NoticeRoles)
	if err != nil || len(emails) == 0 {
		return
	}
	t, _ := p.DB.Tenant.Get(ctx, v.TenantID)
	name := "Your ad agent"
	if agent != nil {
		name = fmt.Sprintf("Your agent \"%s\"", agent.Name)
	}
	body := fmt.Sprintf("%s made a draft for %s, but it will not be posted by itself because %s.\n\nIt waits for your approval in the studio: %s",
		name, offeringName(brief), reason, p.portalLink(t, "/studio/"+v.BriefID.String()))
	for _, email := range emails {
		if err := p.Notif.Send(ctx, v.TenantID.String(), notifications.Message{
			Channel: notifications.ChannelEmail, Recipient: email, Subject: "An ad draft needs your approval", Body: body,
			Metadata:       map[string]string{"source": "marketflow", "kind": "autopilot_held"},
			IdempotencyKey: "autopilot-held-" + v.ID.String() + "-" + email,
		}); err != nil {
			p.logger.Warn("autopilot hold notice not sent", zap.String("version_id", v.ID.String()), zap.Error(err))
		}
	}
}

func offeringName(brief *ent.CreativeBrief) string {
	if brief == nil {
		return "your product"
	}
	if n, ok := brief.Offering["name"].(string); ok && strings.TrimSpace(n) != "" {
		return strings.TrimSpace(n)
	}
	return brief.Title
}

// placeNames are the jobs' destinations, "Facebook (Page name)", sorted.
func placeNames(ctx context.Context, db *ent.Client, jobs []publishing.JobView) []string {
	out := make([]string, 0, len(jobs))
	for _, j := range jobs {
		name := publishing.Title(j.Platform)
		if id, err := parseUUID(j.AccountID); err == nil {
			if acc, err := db.ConnectedAccount.Get(ctx, id); err == nil && acc.DisplayName != "" {
				name += " (" + acc.DisplayName + ")"
			}
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
