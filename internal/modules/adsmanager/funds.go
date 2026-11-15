package adsmanager

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/integrations/meta"
	"github.com/google/uuid"
)

// AccountFunds is what a paid autopilot run reads on the agent's Meta ad account before it makes
// anything (Sprint 20): the same quote the boost wizard and the launch worker build, sized for one
// ad's budget and period.
type AccountFunds struct {
	ConnectedID string `json:"connected_account_id"`
	Quote       Quote  `json:"quote"`
}

var (
	// ErrNoAdAccount means none of the agent's accounts is an active Meta ad account.
	ErrNoAdAccount = errors.New("the agent has no connected Meta ad account")
	// ErrAdAccountToken means the ad account's token cannot be read; it must be connected again.
	ErrAdAccountToken = errors.New("the Meta ad account needs to be connected again")
)

// CheckFunds quotes budget (whole units) over days on the first active Meta ad account among
// connIDs, with a live read of its status and balance on Meta. A Meta error is returned as is.
func CheckFunds(ctx context.Context, db *ent.Client, keys *encryption.KeyProvider, m *meta.Client,
	tenantID uuid.UUID, connIDs []string, budget int64, days int) (AccountFunds, error) {
	ids := make([]uuid.UUID, 0, len(connIDs))
	for _, s := range connIDs {
		if id, err := uuid.Parse(s); err == nil {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return AccountFunds{}, ErrNoAdAccount
	}
	acc, err := db.ConnectedAccount.Query().
		Where(connectedaccount.IDIn(ids...), connectedaccount.TenantIDEQ(tenantID),
			connectedaccount.AccountTypeEQ("meta_ad_account"), connectedaccount.StatusEQ("active")).
		Order(ent.Asc(connectedaccount.FieldCreatedAt)).First(ctx)
	if ent.IsNotFound(err) {
		return AccountFunds{}, ErrNoAdAccount
	}
	if err != nil {
		return AccountFunds{}, err
	}
	out := AccountFunds{ConnectedID: acc.ID.String()}
	if keys == nil || acc.TokenCiphertext == "" {
		return out, ErrAdAccountToken
	}
	tok, err := keys.Decrypt(ctx, acc.TokenCiphertext)
	if err != nil || tok == "" {
		return out, ErrAdAccountToken
	}
	health, err := m.AdAccountHealth(ctx, tok, acc.ExternalID)
	if err != nil {
		return out, err
	}
	out.Quote = BuildQuote(health, budget, days, 0)
	return out, nil
}

// PaidRunReady says whether a paid autopilot run may make its next ad on this quote, and if not,
// why in plain words. A card on threshold billing or a prepaid balance that covers the budget is
// ready. A prepaid account short of the budget, or one with a blocker, is not. A prepaid account
// whose balance Meta does not show is ready to draft: its boost still waits at launch for the
// owner's "I've paid" before anything goes live, so nothing unfunded can start.
func (f AccountFunds) PaidRunReady() (bool, string) {
	q := f.Quote
	if len(q.Blockers) > 0 {
		return false, q.Blockers[0]
	}
	if q.Funding == FundingNeeded {
		return false, fmt.Sprintf("each ad needs %s %d and the ad account has %s %d; add %s %d on Meta",
			q.Currency, q.Budget, q.Currency, q.Balance, q.Currency, q.Shortfall)
	}
	return true, ""
}

// BillingURL is Meta's billing hub for an ad account, where Meta offers M-Pesa on KES accounts.
// The same link is built in marketflow-ui (lib/api/ads.ts metaBillingURL); it is not in Meta's
// API docs, so both change together if Meta moves it.
func BillingURL(adAccountID string) string {
	return "https://business.facebook.com/billing_hub/payment_settings/?asset_id=" +
		url.QueryEscape(strings.TrimPrefix(adAccountID, "act_"))
}
