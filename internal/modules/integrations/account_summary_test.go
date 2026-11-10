package integrations

import (
	"testing"

	"github.com/bengobox/marketflow-api/internal/ent"
)

// Owner request 2026-11-10: a connected card shows each account at a glance (picture, name, id)
// instead of raw provider keys.
func TestAccountSummaries(t *testing.T) {
	rows := []*ent.ConnectedAccount{
		{Platform: PlatformMeta, AccountType: AccountFacebookPage, ExternalID: "1380", DisplayName: "Codevertex Africa", Status: "active"},
		{Platform: PlatformMeta, AccountType: AccountFacebookPage, ExternalID: "2222", DisplayName: "Second Page", Status: "active"},
		{Platform: PlatformMeta, AccountType: AccountInstagramBusiness, ExternalID: "ig1", DisplayName: "codevertex",
			Status: "active", Metadata: map[string]any{"picture_url": "https://cdn.example/ig.jpg"}},
		{Platform: PlatformMeta, AccountType: AccountMetaAdAccount, ExternalID: "act_9", DisplayName: "Ads", Status: "active"},
		{Platform: PlatformTikTok, AccountType: AccountTikTokAdvertiser, ExternalID: "77", DisplayName: "TT", Status: "active"},
		{Platform: PlatformMeta, AccountType: AccountFacebookPage, ExternalID: "gone", DisplayName: "Old", Status: "revoked"},
	}
	got := accountSummaries(rows, "1380")
	meta := got["meta_ads"]
	if len(meta) != 4 {
		t.Fatalf("meta: want 4 active accounts, got %+v", meta)
	}
	if meta[0].ExternalID != "1380" || !meta[0].Default || meta[0].PictureURL != metaGraphURL+"/1380/picture?type=square" {
		t.Errorf("default Page first with its public picture: %+v", meta[0])
	}
	if meta[1].Default || meta[2].PictureURL != "https://cdn.example/ig.jpg" || meta[3].PictureURL != "" {
		t.Errorf("instagram picture kept, ad account has none: %+v", meta)
	}
	if tt := got["tiktok_ads"]; len(tt) != 1 || tt[0].Type != AccountTikTokAdvertiser {
		t.Errorf("tiktok: %+v", tt)
	}
}
