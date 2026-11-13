package adsmanager

import (
	"strings"
	"testing"
	"time"

	"github.com/bengobox/marketflow-api/internal/integrations/meta"
)

func acct(prepay bool, funding string, details *struct {
	ID     string `json:"id"`
	Type   int    `json:"type"`
	Amount string `json:"amount"`
}) *meta.AdAccount {
	return &meta.AdAccount{ID: "act_1", Name: "Boi", AccountStatus: 1, Currency: "KES", IsPrepayAccount: prepay,
		FundingSource: funding, FundingDetails: details, MinDailyBudget: 10000}
}

func TestQuoteFunding(t *testing.T) {
	// A card on threshold billing: no funding step.
	q := BuildQuote(acct(false, "fs1", nil), 5000, 7, 0)
	if q.Funding != FundingCard || !q.Ready(false) || q.Shortfall != 0 {
		t.Fatalf("card: %+v", q)
	}
	// A prepaid balance that covers the budget.
	stored := &struct {
		ID     string `json:"id"`
		Type   int    `json:"type"`
		Amount string `json:"amount"`
	}{Type: meta.FundingStoredBalance, Amount: "KES 6,200.00"}
	q = BuildQuote(acct(true, "", stored), 5000, 7, 10)
	if q.Funding != FundingPrepaid || q.Balance != 6200 || !q.Ready(false) || q.LaunchCredits != 10 {
		t.Fatalf("prepaid: %+v", q)
	}
	// Short: only the shortfall is asked for, and launch waits for the client's top-up.
	stored.Amount = "KES 1,500.00"
	q = BuildQuote(acct(true, "", stored), 5000, 7, 0)
	if q.Funding != FundingNeeded || q.Shortfall != 3500 || q.Ready(false) || !q.Ready(true) {
		t.Fatalf("shortfall: %+v", q)
	}
	// Prepaid with no readable balance: the whole budget, confirmed by the client.
	q = BuildQuote(acct(true, "", nil), 5000, 7, 0)
	if q.Funding != FundingUnknown || q.Shortfall != 5000 || q.Ready(false) {
		t.Fatalf("unknown: %+v", q)
	}
}

// L62: while a boost waits for money, the funding last read on Meta is kept for the Add funds
// step, so the client sees the live balance and shortfall, not the figure from the quote.
func TestFundingSeenFromQuote(t *testing.T) {
	stored := &struct {
		ID     string `json:"id"`
		Type   int    `json:"type"`
		Amount string `json:"amount"`
	}{Type: meta.FundingStoredBalance, Amount: "KES 1,500.00"}
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.FixedZone("EAT", 3*3600))
	q := BuildQuote(acct(true, "", stored), 5000, 7, 0)
	seen := seenFrom(q, now)
	if seen.Funding != FundingNeeded || seen.Balance != 1500 || seen.Shortfall != 3500 || !seen.At.Equal(now) || seen.At.Location() != time.UTC {
		t.Fatalf("seen %+v", seen)
	}
}

func TestQuoteBlockers(t *testing.T) {
	a := acct(false, "fs1", nil)
	a.AccountStatus = 3
	if q := BuildQuote(a, 5000, 7, 0); q.Ready(true) || !strings.Contains(strings.Join(q.Blockers, " "), "unpaid") {
		t.Fatalf("an unsettled account blocks: %+v", q)
	}
	// min_daily_budget 10000 minor = KES 100 a day, so 7 days need KES 700.
	if q := BuildQuote(acct(false, "fs1", nil), 500, 7, 0); q.Ready(true) || !strings.Contains(q.Blockers[0], "700") {
		t.Fatalf("under Meta's minimum: %+v", q)
	}
	b := acct(false, "fs1", nil)
	b.Currency = "XYZ"
	if q := BuildQuote(b, 5000, 7, 0); q.Ready(true) {
		t.Fatal("an unknown currency must block")
	}
	if q := BuildQuote(acct(false, "fs1", nil), 5000, 120, 0); q.Ready(true) {
		t.Fatal("too many days must block")
	}
}

func TestMinorUnits(t *testing.T) {
	if v, _ := ToMinor(5000, "kes"); v != 500000 {
		t.Fatal(v)
	}
	if FromMinor("123456", "KES") != 1234 || FromMinor("bad", "KES") != 0 {
		t.Fatal("FromMinor")
	}
}
