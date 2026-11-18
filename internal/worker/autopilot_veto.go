package worker

import (
	"context"
	"sync"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/modules/custom_agents"
	"github.com/bengobox/marketflow-api/internal/platform/subscriptions"
	"github.com/google/uuid"
)

// AutopilotGate is the ad autopilot's veto-window gate in the worker (Sprint 20, owner
// 2026-11-14): the plan feature autopilot_auto_publish read strictly from subscriptions-api (an
// outage or the demo bypass never grants it) and the tenant's acceptance of the current terms.
// Each run and each pipeline step checks it again, so a lapsed plan falls back to approve-each.
func AutopilotGate(cfg *Config, db *ent.Client) custom_agents.AutoPublishGate {
	custom_agents.SetDefaultVeto(cfg.Autopilot.DefaultVetoHours)
	subs := subscriptions.NewClient(subscriptions.Config{ServiceURL: cfg.Subscriptions.URL, APIKey: cfg.Subscriptions.APIKey})
	return custom_agents.AutoPublishGate{DB: db, HasFeature: subs.StrictHasFeature, TermsVersion: cfg.Autopilot.TermsVersion,
		TermsEffectiveAt: cfg.Autopilot.TermsEffective(), CheckFeature: subs.StrictFeature,
		Terms: workerTermsSource(cfg), TermsPageURL: cfg.Autopilot.TermsPageURL}
}

// VideoFeatures is the video plan gate's entitlement check for the autopilot runner: the same
// ConsumerHasFeature the API's Studio uses (exempt tenants through, fails open on an outage).
func VideoFeatures(cfg *Config) func(ctx context.Context, tenantID, feature string) bool {
	subs := subscriptions.NewClient(subscriptions.Config{ServiceURL: cfg.Subscriptions.URL, APIKey: cfg.Subscriptions.APIKey})
	return subs.ConsumerHasFeature
}

// The worker builds several gates; they share one auth-api terms client (one cache, one last
// known answer) per process.
var (
	workerTermsOnce sync.Once
	workerTerms     custom_agents.TermsSource
)

func workerTermsSource(cfg *Config) custom_agents.TermsSource {
	workerTermsOnce.Do(func() {
		// A typed nil must not become a non-nil interface.
		if c := cfg.Autopilot.TermsSource(cfg.AuthServiceURL, cfg.CrossServiceKey); c != nil {
			workerTerms = c
		}
	})
	return workerTerms
}

// AutopilotLapsedHook tells the autopublish pipeline that a run found the tenant's plan or terms
// no longer allow the veto-window mode, so its posts still waiting are cancelled at once (owner
// 2026-11-14). The pipeline is built after the consumers, so it is bound late (Set); unset, the
// pipeline's sweep does the same within a minute.
type AutopilotLapsedHook struct {
	mu sync.RWMutex
	fn func(ctx context.Context, tenantID uuid.UUID)
}

// Set binds the hook (autopublish.Pipeline.AutoPublishLapsed).
func (h *AutopilotLapsedHook) Set(fn func(ctx context.Context, tenantID uuid.UUID)) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.fn = fn
	h.mu.Unlock()
}

// Call tells the pipeline, when bound.
func (h *AutopilotLapsedHook) Call(ctx context.Context, tenantID uuid.UUID) {
	if h == nil {
		return
	}
	h.mu.RLock()
	fn := h.fn
	h.mu.RUnlock()
	if fn != nil {
		fn(ctx, tenantID)
	}
}
