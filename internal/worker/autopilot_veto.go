package worker

import (
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/modules/custom_agents"
	"github.com/bengobox/marketflow-api/internal/platform/subscriptions"
)

// AutopilotGate is the ad autopilot's veto-window gate in the worker (Sprint 20, owner
// 2026-11-14): the plan feature autopilot_auto_publish read strictly from subscriptions-api (an
// outage or the demo bypass never grants it) and the tenant's acceptance of the current terms.
// Each run and each pipeline step checks it again, so a lapsed plan falls back to approve-each.
func AutopilotGate(cfg *Config, db *ent.Client) custom_agents.AutoPublishGate {
	custom_agents.SetDefaultVeto(cfg.Autopilot.DefaultVetoHours)
	subs := subscriptions.NewClient(subscriptions.Config{ServiceURL: cfg.Subscriptions.URL, APIKey: cfg.Subscriptions.APIKey})
	return custom_agents.AutoPublishGate{DB: db, HasFeature: subs.StrictHasFeature, TermsVersion: cfg.Autopilot.TermsVersion}
}
