package creative

import "testing"

// Owner 2026-11-10: an approved video is removed 48 hours after it has been used (every post it
// was scheduled for is finished and at least one went out, or the client downloaded it), never
// while a post or a boost still needs it.
func TestAfterUseDue(t *testing.T) {
	cases := []struct {
		name       string
		jobs       []string
		downloaded bool
		boostBusy  bool
		want       bool
	}{
		{"never used", nil, false, false, false},
		{"posted", []string{"published"}, false, false, true},
		{"posted and one failed", []string{"published", "failed"}, false, false, true},
		{"only failed: the client may retry", []string{"failed"}, false, false, false},
		{"only cancelled", []string{"cancelled"}, false, false, false},
		{"a post still scheduled", []string{"published", "pending"}, false, false, false},
		{"a post still uploading", []string{"uploading"}, true, false, false},
		{"downloaded, never posted", nil, true, false, true},
		{"downloaded but a boost is launching", nil, true, true, false},
		{"posted but a boost is launching", []string{"published"}, false, true, false},
	}
	for _, c := range cases {
		if got := afterUseDue(c.jobs, c.downloaded, c.boostBusy); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
