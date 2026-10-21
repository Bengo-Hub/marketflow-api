package leads

import (
	"encoding/json"
	"strings"
	"testing"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
)

func strp(s string) *string { return &s }

func TestLeadCreatedEventShape(t *testing.T) {
	tenantID, leadID, campaignID := uuid.New(), uuid.New(), uuid.New()
	l := &ent.Lead{
		ID: leadID, TenantID: tenantID, Source: "meta_ad", LeadType: "customer",
		CampaignID: &campaignID,
		Email:      strp("jane@example.com"), Phone: strp("+254700000000"), FirstName: strp("Jane"),
	}

	ev := leadCreatedEvent(l)
	if got := ev.Subject(); got != "lead.created" {
		t.Fatalf("subject = %q, want lead.created", got)
	}
	if ev.AggregateID != leadID || ev.TenantID != tenantID {
		t.Fatalf("aggregate/tenant = %s/%s, want %s/%s", ev.AggregateID, ev.TenantID, leadID, tenantID)
	}

	raw, err := ev.ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	// The outbox poller rebuilds the event from these exact bytes.
	back, err := eventslib.FromJSON(raw)
	if err != nil || back.Subject() != "lead.created" {
		t.Fatalf("outbox round trip failed: %v", err)
	}

	env, p, err := eventslib.DecodeEvent[LeadCreatedPayload](raw)
	if err != nil {
		t.Fatal(err)
	}
	if env.TenantID != tenantID || p.LeadID != leadID.String() || p.Source != "meta_ad" ||
		p.LeadType != "customer" || p.CampaignID != campaignID.String() || p.FunnelID != "" {
		t.Fatalf("unexpected payload %+v (env tenant %s)", p, env.TenantID)
	}

	// No personal data travels on the bus.
	for _, pii := range []string{"jane@example.com", "+254700000000", "Jane"} {
		if strings.Contains(string(raw), pii) {
			t.Fatalf("event payload leaks personal data %q: %s", pii, raw)
		}
	}
}

func TestLeadCreatedEventOptionalRefs(t *testing.T) {
	l := &ent.Lead{ID: uuid.New(), TenantID: uuid.New(), Source: "organic", LeadType: "recruit"}
	raw, err := leadCreatedEvent(l).ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if _, ok := env.Payload["campaign_id"]; ok {
		t.Fatal("campaign_id must be omitted when the lead has none")
	}
	if _, ok := env.Payload["funnel_id"]; ok {
		t.Fatal("funnel_id must be omitted when the lead has none")
	}
}
