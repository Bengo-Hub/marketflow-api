package worker

import (
	"strings"
	"testing"
)

// Sprint 18 L58 (owner, 2026-11-11): an email to the business per new lead, at most one every 15
// minutes, naming the source and linking to the lead.
func TestLeadAlertText(t *testing.T) {
	subject, body := leadAlertText("Jane Doe", "meta_ad", 3, "https://p/acme/leads/1")
	if subject != "New lead: Jane Doe" {
		t.Fatalf("subject %q", subject)
	}
	for _, want := range []string{"Jane Doe came in from a Meta ad.", "https://p/acme/leads/1", "3 new leads today", "15 minutes"} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	_, one := leadAlertText("A new lead", "custom_source", 1, "x")
	if strings.Contains(one, "new leads today") || !strings.Contains(one, "custom source") {
		t.Errorf("single lead body:\n%s", one)
	}
}

func TestRedisLeadAlertClaimWithoutRedis(t *testing.T) {
	if !redisLeadAlertClaim(nil)(t.Context(), [16]byte{}) {
		t.Fatal("without Redis every lead alerts")
	}
}
