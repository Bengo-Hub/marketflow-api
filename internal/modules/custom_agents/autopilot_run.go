package custom_agents

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/creativebrief"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/google/uuid"
)

// RunBrief is the creative brief one autopilot run starts from (Sprint 20). It is built in code
// from the agent's config: which offering, language and tier this run uses is a rotation by run
// number, never a model's choice, so a run is reproducible and auditable. The creative steps
// that follow (script, storyboard, render) go through the normal gates and credit charges.
type RunBrief struct {
	Title     string
	Offering  map[string]any
	Objective string // the creative module's objective set: sales | leads | awareness | traffic
	Language  string
	Tier      string
	Metadata  map[string]any
}

// CreditsThisMonth sums what an agent's drafts really cost since the 1st (UTC): scripts,
// storyboards and renders alike, from the versions of the briefs it made (the tenant's
// created_at index keeps the brief read small). It is the monthly ceiling's measure.
func CreditsThisMonth(ctx context.Context, db *ent.Client, tenantID, agentID uuid.UUID, now time.Time) (int, error) {
	ids, err := db.CreativeBrief.Query().
		Where(creativebrief.TenantID(tenantID), creativebrief.CreatedAtGTE(monthStart(now)), briefOfAgent(agentID)).IDs(ctx)
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	var totals []struct {
		Sum int `json:"sum"`
	}
	if err := db.CreativeVersion.Query().Where(creativeversion.BriefIDIn(ids...)).
		Aggregate(ent.Sum(creativeversion.FieldCredits)).Scan(ctx, &totals); err != nil || len(totals) == 0 {
		return 0, err
	}
	return totals[0].Sum, nil
}

// creativeObjective maps the autopilot objective onto the creative module's objectives. A
// click-to-WhatsApp ad is a lead ad: the conversation is the lead.
var creativeObjective = map[string]string{"whatsapp": "leads", "leads": "leads", "traffic": "traffic"}

// NextBrief builds run number n (0 for the first run) for the agent. offerings are the
// catalogue snapshots of cfg.OfferingIDs in the same order, as the offering picker returns them;
// a missing snapshot is an error, never a guessed product.
func NextBrief(agentID string, cfg AutopilotConfig, offerings []map[string]any, n int, now time.Time) (RunBrief, error) {
	if len(cfg.OfferingIDs) == 0 || len(offerings) != len(cfg.OfferingIDs) {
		return RunBrief{}, fmt.Errorf("%w: the agent's offerings could not all be loaded", ErrInvalid)
	}
	if n < 0 {
		n = 0
	}
	off := offerings[n%len(offerings)]
	name, _ := off["name"].(string)
	if strings.TrimSpace(name) == "" {
		return RunBrief{}, fmt.Errorf("%w: offering %s has no name", ErrInvalid, cfg.OfferingIDs[n%len(offerings)])
	}
	lang := cfg.Languages[(n/len(offerings))%len(cfg.Languages)]
	tier := cfg.VideoTiers[n%len(cfg.VideoTiers)]
	obj := creativeObjective[cfg.Objective]
	if obj == "" {
		obj = "leads"
	}
	title := fmt.Sprintf("%s (autopilot %s)", name, now.UTC().Format("2 Jan 2006"))
	if len([]rune(title)) > 160 {
		title = string([]rune(title)[:160])
	}
	return RunBrief{
		Title: title, Offering: off, Objective: obj, Language: lang, Tier: tier,
		Metadata: map[string]any{
			"autopilot_agent_id": agentID, "autopilot_run": n, "video_tier": tier,
			"platforms": cfg.Platforms, "approval_mode": cfg.Approval, "paid": cfg.Paid,
		},
	}, nil
}
