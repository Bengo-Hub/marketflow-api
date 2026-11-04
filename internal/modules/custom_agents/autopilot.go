package custom_agents

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Agent kinds (CustomAgent.metadata.kind). Content Autopilot (Sprint 20) adds ad_autopilot and
// content_planner beside the crm agents of Sprint 10; team_member rows belong to the Marketing OS.
const (
	KindCRM            = "crm"
	KindAdAutopilot    = "ad_autopilot"
	KindContentPlanner = "content_planner"
)

// Approval modes. Approve-each is the default; auto-publish needs the plan feature
// autopilot_auto_publish and the tenant's acceptance of the Autopilot terms.
const (
	ApproveEach = "approve_each"
	AutoPublish = "auto_publish"
)

// AutopilotConfig is CustomAgent.metadata.autopilot. Every limit here is enforced in code by the
// run, never only in the prompt.
type AutopilotConfig struct {
	OfferingIDs  []string `json:"offering_ids"`
	Objective    string   `json:"objective"` // whatsapp | leads | traffic
	Languages    []string `json:"languages"`
	VideoTiers   []string `json:"video_tiers"` // template | presenter | presenter_premium
	Cadence      string   `json:"cadence"`     // daily | weekly
	Weekday      int      `json:"weekday"`     // 0 Sunday, for weekly
	Time         string   `json:"time"`        // HH:MM, tenant time
	Platforms    []string `json:"platforms"`   // facebook | instagram | youtube | tiktok
	AccountIDs   []string `json:"connected_account_ids"`
	Paid         bool     `json:"paid"`
	PerAdBudget  int64    `json:"per_ad_budget"`      // whole currency units, paid to Meta by the client
	PeriodDays   int      `json:"period_days"`        // each ad's run
	MonthlyAdCap int64    `json:"monthly_ad_ceiling"` // the most ad money a month, whole units
	Approval     string   `json:"approval_mode"`
	VetoHours    int      `json:"veto_hours"`             // auto-publish heads-up window
	CreditCap    int      `json:"monthly_credit_ceiling"` // AI credits a month
	StopCPL      float64  `json:"stop_cpl"`               // pause above this cost per lead
	EndDate      string   `json:"end_date,omitempty"`     // YYYY-MM-DD, optional
}

var (
	objectiveSet = []string{"whatsapp", "leads", "traffic"}
	tierSet      = []string{"template", "presenter", "presenter_premium"}
	platformSet  = []string{"facebook", "instagram", "youtube", "tiktok"}
	languageSet  = []string{"en", "sw"}
)

// Autopilot limits.
const (
	MaxOfferings  = 20
	DefaultVeto   = 12
	MinVetoHours  = 1
	MaxVetoHours  = 72
	MaxCreditCap  = 5000
	MaxPeriodDays = 90
)

// ErrAutoPublishNotAllowed means the plan or the terms do not allow auto-publish.
var ErrAutoPublishNotAllowed = errors.New("auto-publish needs the Autopilot plan feature and the Autopilot terms")

// Validate checks the config and fills defaults. canAutoPublish is the plan feature
// autopilot_auto_publish together with the tenant's acceptance of the terms.
func (c *AutopilotConfig) Validate(canAutoPublish bool) error {
	var problems []string
	add := func(f string, a ...any) { problems = append(problems, fmt.Sprintf(f, a...)) }

	if len(c.OfferingIDs) == 0 || len(c.OfferingIDs) > MaxOfferings {
		add("choose 1 to %d offerings to rotate", MaxOfferings)
	}
	if !slices.Contains(objectiveSet, c.Objective) {
		add("objective must be whatsapp, leads or traffic")
	}
	if len(c.Languages) == 0 {
		c.Languages = []string{"en"}
	}
	for _, l := range c.Languages {
		if !slices.Contains(languageSet, l) {
			add("language %q is not supported", l)
		}
	}
	if len(c.VideoTiers) == 0 {
		c.VideoTiers = []string{"template"}
	}
	for _, t := range c.VideoTiers {
		if !slices.Contains(tierSet, t) {
			add("video tier %q is not known", t)
		}
	}
	switch c.Cadence {
	case "daily":
	case "weekly":
		if c.Weekday < 0 || c.Weekday > 6 {
			add("weekday must be 0 (Sunday) to 6")
		}
	default:
		add("cadence must be daily or weekly")
	}
	if _, err := time.Parse("15:04", c.Time); err != nil {
		add("time must be HH:MM")
	}
	if len(c.Platforms) == 0 {
		add("choose at least one platform")
	}
	for _, p := range c.Platforms {
		if !slices.Contains(platformSet, p) {
			add("platform %q is not supported", p)
		}
	}
	if len(c.AccountIDs) == 0 {
		add("choose the connected accounts to post to")
	}
	if c.Paid {
		if c.PerAdBudget <= 0 || c.MonthlyAdCap <= 0 {
			add("a paid autopilot needs a per-ad budget and a monthly ceiling")
		} else if c.PerAdBudget > c.MonthlyAdCap {
			add("the per-ad budget is above the monthly ceiling")
		}
		if c.PeriodDays < 1 || c.PeriodDays > MaxPeriodDays {
			add("each ad runs for 1 to %d days", MaxPeriodDays)
		}
	}
	switch c.Approval {
	case "", ApproveEach:
		c.Approval = ApproveEach
	case AutoPublish:
		if !canAutoPublish {
			return ErrAutoPublishNotAllowed
		}
		if c.VetoHours == 0 {
			c.VetoHours = DefaultVeto
		}
		if c.VetoHours < MinVetoHours || c.VetoHours > MaxVetoHours {
			add("the veto window is %d to %d hours", MinVetoHours, MaxVetoHours)
		}
	default:
		add("approval mode must be approve_each or auto_publish")
	}
	if c.CreditCap <= 0 || c.CreditCap > MaxCreditCap {
		add("set a monthly credit ceiling of 1 to %d", MaxCreditCap)
	}
	if c.StopCPL < 0 {
		add("the stop rule cannot be negative")
	}
	if c.EndDate != "" {
		if _, err := time.Parse("2006-01-02", c.EndDate); err != nil {
			add("end date must be YYYY-MM-DD")
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %s", ErrInvalid, strings.Join(problems, "; "))
	}
	return nil
}

// WithinEnvelope reports whether one more paid ad fits the month: spent so far plus the ad's
// budget never goes over the ceiling. Money caps live in code, never in a decision.
func (c AutopilotConfig) WithinEnvelope(spentThisMonth int64) bool {
	return !c.Paid || spentThisMonth+c.PerAdBudget <= c.MonthlyAdCap
}

// CreditsFit reports whether a run's forecast fits the month's credit ceiling.
func (c AutopilotConfig) CreditsFit(usedThisMonth, forecast int) bool {
	return usedThisMonth+forecast <= c.CreditCap
}
