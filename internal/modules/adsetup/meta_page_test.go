package adsetup

import (
	"errors"
	"testing"
)

// A Page missing from the ad account's list is only a fault when the login may not advertise it
// (2026-11-25: the checklist told an owner with full access to fix the Page).
func TestPageLinkIsJudgedByTheLoginsTasks(t *testing.T) {
	in := metaInput{Cfg: Config{}, ActID: "act_1", Pages: []metaPage{{ID: "p1", Name: "Shop"}}}
	for name, c := range map[string]struct {
		promote []string
		tasks   map[string][]string
		err     error
		want    string
	}{
		"listed":          {[]string{"p1"}, nil, nil, StatusOK},
		"may advertise":   {nil, map[string][]string{"p1": {"MANAGE", "ADVERTISE"}}, nil, StatusInfo},
		"may not":         {nil, map[string][]string{"p1": {"ANALYZE"}}, nil, StatusAction},
		"page not listed": {nil, map[string][]string{}, nil, StatusAction},
		"tasks unread":    {nil, nil, errors.New("x"), StatusUnknown},
	} {
		it := in.pageLinkItem(c.promote, nil, c.tasks, c.err)
		if it.Status != c.want {
			t.Errorf("%s: %s", name, it.Status)
		}
		if it.Status == StatusAction && len(it.Points) != 3 {
			t.Errorf("%s: no steps", name)
		}
	}
}
