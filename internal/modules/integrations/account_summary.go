package integrations

import (
	"net/url"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
)

// accountSummary is one connected account as the integrations page shows it.
type accountSummary struct {
	Type       string `json:"type"`
	ExternalID string `json:"external_id"`
	Name       string `json:"name"`
	PictureURL string `json:"picture_url,omitempty"`
	Default    bool   `json:"default,omitempty"`
	// Health is ok, expiring or expired; anything but ok shows a reconnect prompt on the card.
	Health string `json:"health"`
	// ExpiresAt is the renew-by date, sent only while the account is expiring.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// Problem says in plain words why the account needs reconnecting.
	Problem string `json:"problem,omitempty"`
}

// platformProvider maps a connected account's platform to the provider type the page keys on.
var platformProvider = map[string]string{PlatformMeta: "meta_ads", PlatformTikTok: "tiktok_ads"}

var accountTypeOrder = map[string]int{
	AccountFacebookPage: 0, AccountInstagramBusiness: 1, AccountMetaAdAccount: 2, AccountTikTokAdvertiser: 3,
}

// accountSummaries groups a tenant's connected accounts by provider type: the default Page
// first, then Pages, Instagram accounts and ad accounts. Revoked accounts (disconnected) are
// left out; expired ones stay on the card with a reconnect prompt. A Facebook Page's picture is
// Graph's public picture redirect (no token, never expires); an Instagram account uses the
// picture stored when it was connected. authFailures holds the newest publishing error per
// account that needs a reconnect (recentAuthFailures).
func accountSummaries(rows []*ent.ConnectedAccount, defaultPageID string, authFailures map[uuid.UUID]time.Time, now time.Time) map[string][]accountSummary {
	out := map[string][]accountSummary{}
	for _, a := range rows {
		prov, ok := platformProvider[a.Platform]
		if !ok || a.Status == "revoked" {
			continue
		}
		s := accountSummary{Type: a.AccountType, ExternalID: a.ExternalID, Name: a.DisplayName}
		s.Health, s.ExpiresAt, s.Problem = accountHealth(a, authFailures[a.ID], now)
		switch a.AccountType {
		case AccountFacebookPage:
			s.PictureURL = metaGraphURL + "/" + url.PathEscape(a.ExternalID) + "/picture?type=square"
			s.Default = a.ExternalID == defaultPageID
		default:
			if p, ok := a.Metadata["picture_url"].(string); ok {
				s.PictureURL = p
			}
		}
		out[prov] = append(out[prov], s)
	}
	for _, list := range out {
		sort.SliceStable(list, func(i, j int) bool {
			if list[i].Default != list[j].Default {
				return list[i].Default
			}
			return accountTypeOrder[list[i].Type] < accountTypeOrder[list[j].Type]
		})
	}
	return out
}
