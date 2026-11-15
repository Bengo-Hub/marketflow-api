package adsmanager

import (
	"strings"
	"testing"

	"github.com/bengobox/marketflow-api/internal/integrations/meta"
)

// A paid autopilot run goes ahead on a card or a covering balance, waits on a shortfall or a
// blocked account, and drafts on an unknown balance (the boost itself still waits for "I've paid").
func TestPaidRunReady(t *testing.T) {
	stored := func(amount string) *struct {
		ID     string `json:"id"`
		Type   int    `json:"type"`
		Amount string `json:"amount"`
	} {
		return &struct {
			ID     string `json:"id"`
			Type   int    `json:"type"`
			Amount string `json:"amount"`
		}{Type: meta.FundingStoredBalance, Amount: amount}
	}
	cases := []struct {
		name string
		acc  *meta.AdAccount
		ok   bool
		why  string
	}{
		{"card", acct(false, "fs1", nil), true, ""},
		{"covered", acct(true, "", stored("KES 6,200.00")), true, ""},
		{"short", acct(true, "", stored("KES 1,500.00")), false, "add KES 3500 on Meta"},
		{"unknown balance", acct(true, "", nil), true, ""},
	}
	for _, c := range cases {
		ok, why := AccountFunds{Quote: BuildQuote(c.acc, 5000, 7, 0)}.PaidRunReady()
		if ok != c.ok || (c.why != "" && !strings.Contains(why, c.why)) {
			t.Fatalf("%s: ok=%v why=%q", c.name, ok, why)
		}
	}
	blocked := acct(false, "fs1", nil)
	blocked.AccountStatus = 3
	if ok, why := (AccountFunds{Quote: BuildQuote(blocked, 5000, 7, 0)}).PaidRunReady(); ok || !strings.Contains(why, "unpaid balance") {
		t.Fatalf("a blocked account must wait: %v %q", ok, why)
	}
}

func TestBillingURL(t *testing.T) {
	if got := BillingURL("act_123"); got != "https://business.facebook.com/billing_hub/payment_settings/?asset_id=123" {
		t.Fatal(got)
	}
}
