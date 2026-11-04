package custom_agents

import (
	"fmt"
	"strings"
	"time"
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
