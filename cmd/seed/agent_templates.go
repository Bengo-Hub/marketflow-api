package main

import (
	"context"
	"log"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/customagent"
	"github.com/bengobox/marketflow-api/internal/modules/custom_agents"
	"github.com/google/uuid"
)

// agentTemplate is one ready-made ad agent in the store (Sprint 20). A template carries no
// offerings or accounts: the owner picks those after hiring, and a hired copy starts switched off.
type agentTemplate struct {
	Name        string
	Description string
	Category    string
	Industries  []string
	Frequency   string
	Weekday     int
	Time        string
}

func defaultAgentTemplates() []agentTemplate {
	return []agentTemplate{
		{
			Name:        "Weekly product ads",
			Description: "Drafts one video ad a week for your products, in turn, for you to review every Monday morning.",
			Category:    "ads",
			Frequency:   "weekly", Weekday: 1, Time: "07:00",
		},
		{
			Name:        "Weekend promotion ads",
			Description: "Drafts an ad every Thursday, in time to approve and post it before the weekend rush.",
			Category:    "ads",
			Industries:  []string{"retail", "restaurant", "beauty", "fashion"},
			Frequency:   "weekly", Weekday: 4, Time: "09:00",
		},
		{
			Name:        "Daily fresh content",
			Description: "Drafts one ad every morning for busy shops that post daily. Set a monthly credit ceiling that fits your budget.",
			Category:    "ads",
			Frequency:   "daily", Time: "06:30",
		},
	}
}

// seedAgentTemplates adds the store rows under the platform tenant, by name, so a re-run neither
// duplicates them nor overwrites a version the platform owner edited.
func seedAgentTemplates(ctx context.Context, client *ent.Client, tenantID uuid.UUID, scope string) int {
	if scope != "platform" {
		return 0
	}
	count := 0
	for _, t := range defaultAgentTemplates() {
		exists, err := client.CustomAgent.Query().
			Where(customagent.TenantID(tenantID), customagent.NameEQ(t.Name)).
			Exist(ctx)
		if err != nil || exists {
			continue
		}
		trigger := map[string]any{"frequency": t.Frequency, "time": t.Time}
		if t.Frequency == "weekly" {
			trigger["weekday"] = t.Weekday
		}
		if err := custom_agents.ValidateTrigger(custom_agents.TriggerSchedule, trigger); err != nil {
			log.Printf("agent template %q: %v", t.Name, err)
			continue
		}
		meta := map[string]any{
			"kind": custom_agents.KindAdAutopilot,
			"template": map[string]any{
				"category": t.Category, "description": t.Description, "industries": t.Industries, "version": 1,
			},
		}
		if err := client.CustomAgent.Create().
			SetTenantID(tenantID).SetName(t.Name).SetDescription(t.Description).
			SetTriggerType(custom_agents.TriggerSchedule).SetTriggerConfig(trigger).
			SetIsActive(false).SetMetadata(meta).Exec(ctx); err != nil {
			log.Printf("agent template %q: %v", t.Name, err)
			continue
		}
		count++
	}
	return count
}
