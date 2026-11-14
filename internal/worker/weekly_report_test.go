package worker

import (
	"strings"
	"testing"
	"time"

	"github.com/bengobox/marketflow-api/internal/modules/analytics"
)

func TestWeeklyReportBody(t *testing.T) {
	cpl := 250.0
	now := time.Date(2026, 11, 16, 6, 0, 0, 0, time.UTC)
	d := analytics.WeekDigest{From: now.AddDate(0, 0, -7), To: now, Leads: 12, LeadsBefore: 8, Converted: 3,
		DealsWon: 2, WonValue: 45000, Spend: 3000, Currency: "KES", CostPerLead: &cpl,
		TopCampaigns: []analytics.CampaignROI{{Name: "Aloe launch", Leads: 6, Spend: 1200, Currency: "KES", WonDeals: 1}}}
	body := weeklyReportBody(d, "https://example.test/acme/dashboard")
	for _, want := range []string{
		"Here is your week, 9 Nov to 15 Nov.",
		"New leads: 12 (8 the week before)",
		"Deals won: 2, worth KES 45,000.00",
		"Ad spend: KES 3,000.00 (KES 250.00 per lead)",
		"1. Aloe launch: 6 leads, KES 1,200.00 spent, 1 won",
		"https://example.test/acme/dashboard",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body misses %q:\n%s", want, body)
		}
	}
	// Each line is its own paragraph, or the client would fold them into one.
	if strings.Contains(strings.ReplaceAll(body, "\n\n", ""), "\n") {
		t.Fatalf("single line breaks would be folded:\n%s", body)
	}
}

func TestWeekDigestEmpty(t *testing.T) {
	if !(analytics.WeekDigest{}).Empty() {
		t.Fatal("a week with nothing is empty")
	}
	if (analytics.WeekDigest{Leads: 1}).Empty() {
		t.Fatal("a lead is news")
	}
}
