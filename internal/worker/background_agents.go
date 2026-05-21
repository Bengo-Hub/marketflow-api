package worker

// background_agents.go contains Vera's autonomous background agents that run
// on cron schedules or NATS event triggers within the marketflow-worker.
//
// Each agent is a separate Handler implementation that integrates with the
// marketflow-api via S2S calls using INTERNAL_SERVICE_KEY.
//
// Agents:
//   - LeadScorerAgent          triggered by crm.lead.created + crm.activity.logged
//   - ProactiveFollowUpAgent   hourly cron — creates tasks for stale leads/contacts
//   - DealStaleDetector        daily cron — flags deals with no updates in 14+ days
//   - NurtureSequenceEnhancer  triggered by crm.lead.qualified

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/contact"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"
)

// agentS2SPost is a small helper for JSON POST calls with X-API-Key auth.
func agentS2SPost(ctx context.Context, hc *http.Client, url, serviceKey string, body map[string]any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", serviceKey)
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		rb, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("s2s %s → %d: %s", url, resp.StatusCode, truncateStr(string(rb), 200))
	}
	return nil
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ── LeadScorerAgent ─────────────────────────────────────────────────────────

// LeadScorerAgent listens to crm.lead.created and crm.activity.logged.
// It calls the lead rescore endpoint and, if the score crosses 70, publishes
// crm.lead.qualified back to NATS.
type LeadScorerAgent struct {
	db         *ent.Client
	apiBaseURL string
	serviceKey string
	js         jetstream.JetStream
	logger     *zap.Logger
	hc         *http.Client
}

func (a *LeadScorerAgent) Subject() string { return "crm.lead.created" }
func (a *LeadScorerAgent) Durable() string { return "marketflow-worker-agent-lead-scorer" }

func (a *LeadScorerAgent) Handle(ctx context.Context, msg jetstream.Msg) error {
	var ev struct {
		TenantID string `json:"tenant_id"`
		LeadID   string `json:"lead_id"`
	}
	if err := json.Unmarshal(msg.Data(), &ev); err != nil {
		return fmt.Errorf("decode crm.lead.created: %w", err)
	}
	if ev.LeadID == "" || a.apiBaseURL == "" {
		return nil
	}
	return a.rescore(ctx, ev.TenantID, ev.LeadID)
}

func (a *LeadScorerAgent) rescore(ctx context.Context, tenantID, leadID string) error {
	url := strings.TrimSuffix(a.apiBaseURL, "/") + "/api/v1/leads/" + leadID + "/rescore"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", a.serviceKey)
	req.Header.Set("X-Tenant-ID", tenantID)
	resp, err := a.hc.Do(req)
	if err != nil {
		a.logger.Warn("lead rescore request failed", zap.String("lead_id", leadID), zap.Error(err))
		return nil // fail-open
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)

	var result struct {
		Score    int `json:"score"`
		OldScore int `json:"old_score"`
	}
	if err := json.Unmarshal(b, &result); err != nil {
		return nil
	}

	// Publish qualified event if score just crossed the threshold
	if result.Score >= 70 && result.OldScore < 70 && a.js != nil {
		payload, _ := json.Marshal(map[string]any{
			"tenant_id": tenantID,
			"lead_id":   leadID,
			"score":     result.Score,
		})
		if _, err := a.js.Publish(ctx, "crm.lead.qualified", payload); err != nil {
			a.logger.Warn("publish crm.lead.qualified failed", zap.Error(err))
		} else {
			a.logger.Info("lead qualified — published crm.lead.qualified",
				zap.String("lead_id", leadID), zap.Int("score", result.Score))
		}
	}
	return nil
}

// ActivityLoggedLeadScorerAgent handles crm.activity.logged for lead rescoring.
type ActivityLoggedLeadScorerAgent struct {
	*LeadScorerAgent
}

func (a *ActivityLoggedLeadScorerAgent) Subject() string { return "crm.activity.logged" }
func (a *ActivityLoggedLeadScorerAgent) Durable() string {
	return "marketflow-worker-agent-activity-lead-scorer"
}

func (a *ActivityLoggedLeadScorerAgent) Handle(ctx context.Context, msg jetstream.Msg) error {
	var ev struct {
		TenantID   string `json:"tenant_id"`
		EntityType string `json:"entity_type"`
		EntityID   string `json:"entity_id"`
	}
	if err := json.Unmarshal(msg.Data(), &ev); err != nil {
		return nil
	}
	if ev.EntityType != "lead" {
		return nil
	}
	return a.rescore(ctx, ev.TenantID, ev.EntityID)
}

// ── ProactiveFollowUpAgent ───────────────────────────────────────────────────

// ProactiveFollowUpAgent is a CronHandler (implements Subject() "cron.hourly").
// It queries leads and contacts with no recent activity and creates follow-up tasks.
type ProactiveFollowUpAgent struct {
	db         *ent.Client
	apiBaseURL string
	serviceKey string
	logger     *zap.Logger
	hc         *http.Client
}

func (a *ProactiveFollowUpAgent) Subject() string { return "cron.followup.hourly" }
func (a *ProactiveFollowUpAgent) Durable() string { return "marketflow-worker-agent-followup" }

func (a *ProactiveFollowUpAgent) Handle(ctx context.Context, msg jetstream.Msg) error {
	if a.db == nil || a.apiBaseURL == "" {
		return nil
	}

	var ev struct {
		TenantID     string `json:"tenant_id"`
		FollowUpDays int    `json:"follow_up_days"`
	}
	if err := json.Unmarshal(msg.Data(), &ev); err != nil {
		return nil
	}
	if ev.FollowUpDays <= 0 {
		ev.FollowUpDays = 7
	}

	tenantID, err := uuid.Parse(ev.TenantID)
	if err != nil {
		return nil
	}

	cutoff := time.Now().AddDate(0, 0, -ev.FollowUpDays)
	leads, _ := a.db.Lead.Query().
		Where(
			lead.TenantIDEQ(tenantID),
			lead.StatusEQ("active"),
			lead.UpdatedAtLT(cutoff),
		).
		Limit(50).
		All(ctx)

	base := strings.TrimSuffix(a.apiBaseURL, "/")
	for _, l := range leads {
		title := fmt.Sprintf("Follow up — no contact in %d days", ev.FollowUpDays)
		due := time.Now().Add(24 * time.Hour)
		_ = agentS2SPost(ctx, a.hc, base+"/api/v1/tasks", a.serviceKey, map[string]any{
			"entity_type": "lead",
			"entity_id":   l.ID.String(),
			"title":       title,
			"due_at":      due.Format(time.RFC3339),
			"priority":    "normal",
			"tenant_id":   ev.TenantID,
		})
	}

	contacts, _ := a.db.Contact.Query().
		Where(
			contact.TenantIDEQ(tenantID),
			contact.UpdatedAtLT(cutoff),
		).
		Limit(50).
		All(ctx)

	for _, c := range contacts {
		title := fmt.Sprintf("Follow up — no contact in %d days", ev.FollowUpDays)
		due := time.Now().Add(24 * time.Hour)
		_ = agentS2SPost(ctx, a.hc, base+"/api/v1/tasks", a.serviceKey, map[string]any{
			"entity_type": "contact",
			"entity_id":   c.ID.String(),
			"title":       title,
			"due_at":      due.Format(time.RFC3339),
			"priority":    "normal",
			"tenant_id":   ev.TenantID,
		})
	}

	a.logger.Info("proactive followup agent ran",
		zap.String("tenant", ev.TenantID),
		zap.Int("leads_queued", len(leads)),
		zap.Int("contacts_queued", len(contacts)),
	)
	return nil
}

// ── DealStaleDetector ────────────────────────────────────────────────────────

// DealStaleDetector is triggered daily (via NATS cron.daily event).
// It queries open deals not updated in 14+ days and creates stale tasks.
type DealStaleDetector struct {
	apiBaseURL string
	serviceKey string
	logger     *zap.Logger
	hc         *http.Client
}

func (a *DealStaleDetector) Subject() string { return "cron.deal.stale.daily" }
func (a *DealStaleDetector) Durable() string { return "marketflow-worker-agent-deal-stale" }

func (a *DealStaleDetector) Handle(ctx context.Context, msg jetstream.Msg) error {
	if a.apiBaseURL == "" {
		return nil
	}

	var ev struct {
		TenantID  string `json:"tenant_id"`
		StaleDays int    `json:"stale_days"`
	}
	if err := json.Unmarshal(msg.Data(), &ev); err != nil {
		return nil
	}
	if ev.StaleDays <= 0 {
		ev.StaleDays = 14
	}

	base := strings.TrimSuffix(a.apiBaseURL, "/")
	url := fmt.Sprintf("%s/api/v1/deals?status=open&stale_days=%d&tenant_id=%s&limit=50",
		base, ev.StaleDays, ev.TenantID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("X-API-Key", a.serviceKey)
	req.Header.Set("X-Tenant-ID", ev.TenantID)
	resp, err := a.hc.Do(req)
	if err != nil || resp.StatusCode >= 300 {
		return nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)

	var result struct {
		Data []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &result); err != nil {
		return nil
	}

	due := time.Now().Add(24 * time.Hour)
	for _, d := range result.Data {
		title := fmt.Sprintf("Stale deal — needs attention: %s", d.Title)
		_ = agentS2SPost(ctx, a.hc, base+"/api/v1/tasks", a.serviceKey, map[string]any{
			"entity_type": "deal",
			"entity_id":   d.ID,
			"title":       title,
			"due_at":      due.Format(time.RFC3339),
			"priority":    "high",
			"tenant_id":   ev.TenantID,
		})
		body := "Deal flagged as stale by Vera AI (no activity in 14+ days)"
		_ = agentS2SPost(ctx, a.hc, base+"/api/v1/activities", a.serviceKey, map[string]any{
			"entity_type":   "deal",
			"entity_id":     d.ID,
			"activity_type": "note",
			"body":          body,
			"source":        "agent",
			"tenant_id":     ev.TenantID,
		})
	}

	if len(result.Data) > 0 {
		a.logger.Info("deal stale detector ran",
			zap.String("tenant", ev.TenantID),
			zap.Int("stale_deals", len(result.Data)),
		)
	}
	return nil
}

// ── NurtureSequenceEnhancer ──────────────────────────────────────────────────

// NurtureSequenceEnhancer triggers on crm.lead.qualified and enrolls
// the lead in the most appropriate nurture sequence.
type NurtureSequenceEnhancer struct {
	db         *ent.Client
	apiBaseURL string
	serviceKey string
	logger     *zap.Logger
	hc         *http.Client
}

func (a *NurtureSequenceEnhancer) Subject() string { return "crm.lead.qualified" }
func (a *NurtureSequenceEnhancer) Durable() string { return "marketflow-worker-agent-nurture-enhancer" }

func (a *NurtureSequenceEnhancer) Handle(ctx context.Context, msg jetstream.Msg) error {
	var ev struct {
		TenantID string `json:"tenant_id"`
		LeadID   string `json:"lead_id"`
		Score    int    `json:"score"`
	}
	if err := json.Unmarshal(msg.Data(), &ev); err != nil {
		return nil
	}
	if ev.LeadID == "" || a.db == nil {
		return nil
	}

	tenantID, err := uuid.Parse(ev.TenantID)
	if err != nil {
		return nil
	}
	leadID, err := uuid.Parse(ev.LeadID)
	if err != nil {
		return nil
	}
	_ = tenantID
	_ = leadID

	// Find best matching nurture sequence for this qualified lead via S2S
	base := strings.TrimSuffix(a.apiBaseURL, "/")
	seqURL := fmt.Sprintf("%s/api/v1/sequences?trigger_event=lead.qualified&active=true&limit=1&tenant_id=%s",
		base, ev.TenantID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, seqURL, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("X-API-Key", a.serviceKey)
	req.Header.Set("X-Tenant-ID", ev.TenantID)
	resp, err := a.hc.Do(req)
	if err != nil || resp.StatusCode >= 300 {
		return nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)

	var seqResult struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &seqResult); err != nil || len(seqResult.Data) == 0 {
		return nil
	}

	// Enroll lead in the sequence
	enrollURL := base + "/api/v1/sequences/" + seqResult.Data[0].ID + "/enroll"
	if err := agentS2SPost(ctx, a.hc, enrollURL, a.serviceKey, map[string]any{
		"lead_id":   ev.LeadID,
		"tenant_id": ev.TenantID,
		"source":    "vera-qualified-agent",
	}); err != nil {
		a.logger.Warn("failed to enroll qualified lead in sequence",
			zap.String("lead_id", ev.LeadID), zap.Error(err))
		return nil
	}

	a.logger.Info("qualified lead enrolled in nurture sequence",
		zap.String("lead_id", ev.LeadID),
		zap.String("sequence_id", seqResult.Data[0].ID),
	)
	return nil
}
