package contacts

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/activity"
	"github.com/bengobox/marketflow-api/internal/ent/contact"
	"github.com/bengobox/marketflow-api/internal/ent/deal"
	"github.com/bengobox/marketflow-api/internal/ent/nurtureenrollment"
	"github.com/bengobox/marketflow-api/internal/ent/scheduledmeeting"
	"github.com/bengobox/marketflow-api/internal/ent/task"
	"github.com/google/uuid"
)

// View360Config holds optional S2S URLs for cross-service data enrichment.
type View360Config struct {
	OrderingURL    string
	POSURL         string
	TreasuryURL    string
	ServiceKey     string
	HTTPClient     *http.Client
}

// Contact360Response is the full 360° view of a CRM contact.
type Contact360Response struct {
	Contact        *ent.Contact     `json:"contact"`
	OpenDeals      []*ent.Deal      `json:"open_deals"`
	WonDeals       []*ent.Deal      `json:"won_deals"`
	Activities     []*ent.Activity  `json:"activities"`
	OpenTasks      []*ent.Task      `json:"open_tasks"`
	Meetings       []*ent.ScheduledMeeting  `json:"meetings"`
	NurtureActive  []*ent.NurtureEnrollment `json:"nurture_active"`
	OrderSummary   map[string]any   `json:"order_summary,omitempty"`
	PaymentSummary map[string]any   `json:"payment_summary,omitempty"`
	LoyaltySummary map[string]any   `json:"loyalty_summary,omitempty"`
}

// Get360 returns the full 360° profile for a contact, enriched with cross-service data.
func (s *Service) Get360(ctx context.Context, tenantID, contactID uuid.UUID, cfg View360Config) (*Contact360Response, error) {
	c, err := s.client.Contact.Query().
		Where(contact.ID(contactID), contact.TenantID(tenantID)).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("get contact: %w", err)
	}

	resp := &Contact360Response{Contact: c}

	// Open deals
	resp.OpenDeals, _ = s.client.Deal.Query().
		Where(deal.TenantIDEQ(tenantID), deal.ContactIDEQ(contactID), deal.StatusEQ("open")).
		Order(ent.Desc(deal.FieldCreatedAt)).
		Limit(20).
		All(ctx)

	// Won deals (last 10)
	resp.WonDeals, _ = s.client.Deal.Query().
		Where(deal.TenantIDEQ(tenantID), deal.ContactIDEQ(contactID), deal.StatusEQ("won")).
		Order(ent.Desc(deal.FieldCreatedAt)).
		Limit(10).
		All(ctx)

	// Activity timeline (last 50, newest first)
	resp.Activities, _ = s.client.Activity.Query().
		Where(activity.TenantIDEQ(tenantID), activity.EntityTypeEQ("contact"), activity.EntityIDEQ(contactID)).
		Order(ent.Desc(activity.FieldPerformedAt)).
		Limit(50).
		All(ctx)

	// Open tasks
	resp.OpenTasks, _ = s.client.Task.Query().
		Where(task.TenantIDEQ(tenantID), task.EntityTypeEQ("contact"), task.EntityIDEQ(contactID), task.StatusNEQ("done")).
		Order(ent.Asc(task.FieldDueAt)).
		Limit(20).
		All(ctx)

	// Scheduled meetings (upcoming + last 5 past)
	now := time.Now()
	upcoming, _ := s.client.ScheduledMeeting.Query().
		Where(scheduledmeeting.TenantIDEQ(tenantID), scheduledmeeting.ScheduledAtGT(now)).
		Order(ent.Asc(scheduledmeeting.FieldScheduledAt)).
		Limit(5).
		All(ctx)
	past, _ := s.client.ScheduledMeeting.Query().
		Where(scheduledmeeting.TenantIDEQ(tenantID), scheduledmeeting.ScheduledAtLTE(now)).
		Order(ent.Desc(scheduledmeeting.FieldScheduledAt)).
		Limit(5).
		All(ctx)
	resp.Meetings = append(upcoming, past...)

	// Active nurture enrollments
	resp.NurtureActive, _ = s.client.NurtureEnrollment.Query().
		Where(
			nurtureenrollment.TenantIDEQ(tenantID),
			nurtureenrollment.StatusEQ("active"),
		).
		All(ctx)

	// S2S enrichment — all fail-open with 2s timeout
	if cfg.ServiceKey != "" {
		s2sCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		hc := cfg.HTTPClient
		if hc == nil {
			hc = &http.Client{Timeout: 2 * time.Second}
		}
		contactIDStr := contactID.String()

		if cfg.OrderingURL != "" {
			resp.OrderSummary = s2sGet(s2sCtx, hc, cfg.OrderingURL+
				"/api/v1/orders?crm_contact_id="+contactIDStr+"&limit=5&sort=created_at:desc", cfg.ServiceKey)
		}
		if cfg.TreasuryURL != "" {
			resp.PaymentSummary = s2sGet(s2sCtx, hc, cfg.TreasuryURL+
				"/api/v1/payment-intents?crm_contact_id="+contactIDStr+"&status=succeeded&aggregate=true", cfg.ServiceKey)
		}
		if cfg.POSURL != "" {
			resp.LoyaltySummary = s2sGet(s2sCtx, hc, cfg.POSURL+
				"/api/v1/loyalty-accounts?crm_contact_id="+contactIDStr, cfg.ServiceKey)
		}
	}

	return resp, nil
}

// s2sGet does a fail-open GET to a sibling service and returns the JSON body as map.
func s2sGet(ctx context.Context, hc *http.Client, url, serviceKey string) map[string]any {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("X-API-Key", serviceKey)
	resp, err := hc.Do(req)
	if err != nil || resp.StatusCode >= 300 {
		return nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

// ByEmail finds a contact by email within a tenant (S2S minimal lookup).
func (s *Service) ByEmail(ctx context.Context, tenantID uuid.UUID, email string) (*ent.Contact, error) {
	c, err := s.client.Contact.Query().
		Where(contact.TenantID(tenantID), contact.EmailEQ(strings.ToLower(email))).
		First(ctx)
	if err != nil {
		return nil, fmt.Errorf("contact not found: %w", err)
	}
	return c, nil
}
