// Package adsmanager boosts an approved video on the client's own Meta ad account (Sprint 18).
// Ad money goes from the client to Meta; MarketFlow quotes, checks funding, launches PAUSED,
// tracks review and goes live. API contracts and what is not confirmed:
// marketflow/docs/integrations/meta-paid-ads-apis.md.
package adsmanager

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/bengobox/marketflow-api/internal/integrations/meta"
)

// minorUnits is each currency's Meta offset: budgets are sent in the minor unit (cents). KES and
// USD use 100. The sandbox run confirms it for KES before the switch goes on.
var minorUnits = map[string]int64{"KES": 100, "USD": 100, "EUR": 100, "GBP": 100, "UGX": 1, "TZS": 100, "JPY": 1}

// ToMinor turns a whole-unit amount into Meta's minor units; an unknown currency is an error so
// a budget is never sent a hundred times too small or large.
func ToMinor(amount int64, currency string) (int64, error) {
	off, ok := minorUnits[strings.ToUpper(currency)]
	if !ok {
		return 0, fmt.Errorf("currency %s is not supported for ads yet", currency)
	}
	return amount * off, nil
}

// FromMinor turns Meta's minor-unit string ("123456") into whole units, rounding down.
func FromMinor(v, currency string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return 0
	}
	off := minorUnits[strings.ToUpper(currency)]
	if off == 0 {
		off = 100
	}
	return n / off
}

// Funding says how the client pays Meta for this budget.
type Funding string

const (
	// FundingCard: a card on threshold billing; Meta bills after delivery, no top-up step.
	FundingCard Funding = "card"
	// FundingPrepaid: a prepaid balance that already covers the budget.
	FundingPrepaid Funding = "prepaid"
	// FundingNeeded: a prepaid account short of the budget; the client tops up on Meta.
	FundingNeeded Funding = "needs_funds"
	// FundingUnknown: prepaid, but Meta's docs do not confirm a balance field; the client
	// confirms "I've paid" and launch waits for that.
	FundingUnknown Funding = "unknown"
)

// Quote is gate G7: what the boost costs and what must happen before launch.
type Quote struct {
	Currency      string   `json:"currency"`
	Budget        int64    `json:"budget"` // whole units, paid to Meta by the client
	Days          int      `json:"days"`
	Balance       int64    `json:"balance"`   // prepaid stored balance seen, whole units
	Shortfall     int64    `json:"shortfall"` // what to add on Meta, whole units
	Funding       Funding  `json:"funding"`
	LaunchCredits int      `json:"launch_credits"` // MarketFlow's charge, credits
	Blockers      []string `json:"blockers,omitempty"`
	AccountName   string   `json:"account_name"`
	AccountID     string   `json:"ad_account_id"`
}

// Limits for a boost.
const (
	MinDays = 1
	MaxDays = 90
)

// BuildQuote checks the account and sizes the funding step. budget is in whole currency units.
// launchCredits is 0 for subscribers and the platform's fixed charge otherwise.
func BuildQuote(a *meta.AdAccount, budget int64, days, launchCredits int) Quote {
	q := Quote{Currency: a.Currency, Budget: budget, Days: days, LaunchCredits: launchCredits, AccountName: a.Name, AccountID: a.ID}
	if a.AccountStatus != meta.AccountActive {
		q.Blockers = append(q.Blockers, accountProblem(a))
	}
	if _, err := ToMinor(1, a.Currency); err != nil {
		q.Blockers = append(q.Blockers, err.Error())
	}
	if days < MinDays || days > MaxDays {
		q.Blockers = append(q.Blockers, fmt.Sprintf("a boost runs for %d to %d days", MinDays, MaxDays))
	}
	if budget <= 0 {
		q.Blockers = append(q.Blockers, "choose a budget")
	} else if a.MinDailyBudget > 0 && days > 0 {
		// min_daily_budget is in minor units like every budget field.
		if min := FromMinor(strconv.FormatInt(a.MinDailyBudget, 10), a.Currency) * int64(days); budget < min {
			q.Blockers = append(q.Blockers, fmt.Sprintf("Meta needs at least %s %d for %d days on this account", a.Currency, min, days))
		}
	}
	switch {
	case !a.IsPrepayAccount && a.FundingSource != "":
		q.Funding = FundingCard
	case a.FundingDetails != nil && a.FundingDetails.Type == meta.FundingStoredBalance && a.FundingDetails.Amount != "":
		q.Balance = parseAmount(a.FundingDetails.Amount)
		if q.Balance >= budget {
			q.Funding = FundingPrepaid
		} else {
			q.Funding, q.Shortfall = FundingNeeded, budget-q.Balance
		}
	default:
		q.Funding, q.Shortfall = FundingUnknown, budget
	}
	return q
}

// parseAmount reads funding_source_details.amount, which Meta returns as display text such as
// "KES 1,250.00" (not confirmed); digits before the decimal point count.
func parseAmount(s string) int64 {
	var digits strings.Builder
	for _, r := range s {
		if r == '.' {
			break
		}
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	n, _ := strconv.ParseInt(digits.String(), 10, 64)
	return n
}

func accountProblem(a *meta.AdAccount) string {
	switch a.AccountStatus {
	case 2:
		return "Meta has disabled this ad account; check Account Quality in Meta Business Suite"
	case 3, 8:
		return "this ad account has an unpaid balance with Meta; settle it first"
	case 7:
		return "Meta is reviewing this ad account; try again once it is active"
	case 9:
		return "this ad account is in Meta's grace period; settle the balance first"
	case 100, 101:
		return "this ad account is closed"
	}
	return "this ad account is not active"
}

// Ready reports whether launch may start: no blockers, and the money is in place or confirmed.
func (q Quote) Ready(paidConfirmed bool) bool {
	if len(q.Blockers) > 0 {
		return false
	}
	switch q.Funding {
	case FundingCard, FundingPrepaid:
		return true
	case FundingNeeded, FundingUnknown:
		return paidConfirmed
	}
	return false
}
