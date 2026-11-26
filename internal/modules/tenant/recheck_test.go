package tenant

import "testing"

// A non-active tenant is read again from auth-api at most once a minute (never on every request).
func TestRecheckIsBounded(t *testing.T) {
	s := &Syncer{}
	if !s.dueRecheck("a") || s.dueRecheck("a") || !s.dueRecheck("b") {
		t.Fatal("recheck not bounded per tenant")
	}
}
