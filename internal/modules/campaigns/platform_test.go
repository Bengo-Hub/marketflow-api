package campaigns

import (
	"testing"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/google/uuid"
)

// A paid boost runs on the ad platform: pausing it only in our database would leave it spending
// on Meta while the insights job (which reads active campaigns) stops watching it.
func TestPlatformManagedCampaignsAreNotPausedLocally(t *testing.T) {
	acc, mc, empty := uuid.New(), "120210000000001", ""
	cases := []struct {
		name string
		c    *ent.Campaign
		want bool
	}{
		{"plain campaign", &ent.Campaign{}, false},
		{"boost on a connected ad account", &ent.Campaign{ConnectedAccountID: &acc}, true},
		{"campaign with a Meta id", &ent.Campaign{MetaCampaignID: &mc}, true},
		{"empty Meta id", &ent.Campaign{MetaCampaignID: &empty}, false},
	}
	for _, c := range cases {
		if got := platformManaged(c.c); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
