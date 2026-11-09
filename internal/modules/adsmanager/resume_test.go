package adsmanager

import "testing"

// Only a boost that went live on Meta and was paused can be resumed there; one paused before it
// went live has nothing on Meta to resume, and an active or failed boost is not paused.
func TestResumable(t *testing.T) {
	cases := []struct {
		status, metaID string
		ok             bool
	}{
		{CampaignPaused, "120210000000001", true},
		{CampaignPaused, "", false},
		{CampaignActive, "120210000000001", false},
		{CampaignFailed, "120210000000001", false},
		{CampaignLaunching, "", false},
	}
	for _, c := range cases {
		if err := resumable(c.status, c.metaID); (err == nil) != c.ok {
			t.Errorf("resumable(%q, %q) = %v, want ok=%v", c.status, c.metaID, err, c.ok)
		}
	}
}
