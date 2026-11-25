package adaccounts

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
)

func acc(ext, name, currency string, business any) *ent.ConnectedAccount {
	md := map[string]any{"currency": currency}
	if business != nil {
		md[BusinessID] = business
	}
	return &ent.ConnectedAccount{ID: uuid.New(), AccountType: TypeOf[Meta], ExternalID: ext, DisplayName: name, Metadata: md}
}

// stored is what a saved value reads back as after JSON.
func stored(t *testing.T, v Prefs) Prefs {
	t.Helper()
	b, _ := json.Marshal(map[string]any{TenantKey: v})
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return Read(m)
}

func TestPortfolioBeatsPersonal(t *testing.T) {
	personal := acc("act_1", "A Dev", "KES", "")
	unknown := acc("act_2", "B", "KES", nil)
	business := acc("act_3", "C Africa", "USD", "77")
	all := []*ent.ConnectedAccount{personal, unknown, business}
	if got := (Prefs{}).Pick(Meta, all, ""); got != business {
		t.Fatalf("picked %s", got.DisplayName)
	}
	if got := (Prefs{}).Pick(Meta, all[:2], ""); got != unknown {
		t.Fatalf("picked %s", got.DisplayName)
	}
	if got := (Prefs{}).Pick(Meta, all, personal.ID.String()); got != personal {
		t.Fatal("the earlier WhatsApp boost choice must still count")
	}
	if (Prefs{}).Pick(Meta, nil, "") != nil {
		t.Fatal("no accounts")
	}
}

func TestChoiceSurvivesReconnect(t *testing.T) {
	yes := true
	business := acc("act_3", "C", "USD", "77")
	v, err := Prefs{}.Set(Meta, "act_1", &yes, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A reconnect makes new rows with the same account ids.
	again := []*ent.ConnectedAccount{acc("act_3", "C", "USD", "77"), acc("act_1", "A", "KES", "")}
	if got := stored(t, v).Pick(Meta, again, business.ID.String()); got.ExternalID != "act_1" {
		t.Fatalf("picked %s", got.ExternalID)
	}
}

func TestHideOne(t *testing.T) {
	yes, no := true, false
	v, _ := Prefs{}.Set(Meta, "act_1", &yes, nil)
	v, err := stored(t, v).Set(Meta, "act_1", nil, &yes)
	if err != nil {
		t.Fatal(err)
	}
	p := stored(t, v)
	if p[Meta].Default != "" || !p.Hidden(Meta, "act_1") || p.Hidden(Google, "act_1") {
		t.Fatalf("prefs %+v", p)
	}
	if p.Pick(Meta, []*ent.ConnectedAccount{acc("act_1", "A", "KES", "7")}, "") != nil {
		t.Fatal("a hidden account was picked")
	}
	v, _ = p.Set(Meta, "act_1", nil, &no)
	if stored(t, v).Hidden(Meta, "act_1") {
		t.Fatal("still hidden")
	}
	v, _ = p.Set(Meta, "act_1", &yes, nil)
	if q := stored(t, v); q.Hidden(Meta, "act_1") || q[Meta].Default != "act_1" {
		t.Fatal("making it the default must show it")
	}
	if _, err := p.Set(Meta, "act_1", &yes, &yes); err == nil {
		t.Fatal("hidden and default")
	}
	if _, err := p.Set("x", "1", &yes, nil); err == nil {
		t.Fatal("unknown platform")
	}
}

func TestOwner(t *testing.T) {
	if Owner(acc("a", "", "", nil)) != OwnerUnknown || Owner(acc("a", "", "", "")) != OwnerPersonal || Owner(acc("a", "", "", "5")) != OwnerPortfolio {
		t.Fatal("owner")
	}
	if Owner(&ent.ConnectedAccount{AccountType: TypeOf[Google]}) != OwnerUnknown {
		t.Fatal("google has no owner kind")
	}
}
