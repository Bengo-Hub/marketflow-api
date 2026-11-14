package worker

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/modules/creative"
)

func TestTenantPickList(t *testing.T) {
	tenants := []assistantTenant{{ID: uuid.New(), Name: "Boi Salon"}, {ID: uuid.New(), Name: "A business name that is far too long for a row"}}
	ia := tenantPickList(tenants)
	if ia == nil || ia.List == nil || len(ia.List.Rows) != 2 || ia.List.Rows[0].ID != "mf:t:1" || ia.List.Rows[0].Title != "Boi Salon" {
		t.Fatalf("pick list: %+v", ia)
	}
	if n := len([]rune(ia.List.Rows[1].Title)); n > 24 {
		t.Fatalf("row title is %d characters", n)
	}
	var many []assistantTenant
	for i := 0; i < 11; i++ {
		many = append(many, assistantTenant{ID: uuid.New(), Name: fmt.Sprintf("Shop %d", i)})
	}
	if tenantPickList(many) != nil || tenantPickList(tenants[:1]) != nil {
		t.Fatal("over 10 businesses, or only one, gets no list")
	}
	for id, want := range map[string]int{"mf:t:1": 1, "mf:t:10": 10} {
		if n, ok := tenantPickRow(id); !ok || n != want {
			t.Fatalf("tenantPickRow(%q) = %d, %v", id, n, ok)
		}
	}
	for _, id := range []string{"", "mf:t:", "mf:t:0", "mf:t:x", "mf:yes:r1:4821"} {
		if _, ok := tenantPickRow(id); ok {
			t.Fatalf("tenantPickRow(%q) accepted", id)
		}
	}
}

// The sign-up reply needs the rich sends on and a marketing message; the guards return before
// any Redis or send, so nil clients are safe here.
func TestSignupReplyGuards(t *testing.T) {
	h := NewWhatsAppAssistantHandler(nil, nil, nil, "", "", "", "", "https://mf.example", "codevertex", zap.NewNop())
	in := waInbound{From: "254700000001", WaMessageID: "w1", Text: "make me a tiktok video ad"}
	if err := h.signupReply(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	h.WithRich(creative.DefaultWhatsAppConfig(), "", 0)
	if !h.rich || h.window() != creative.DefaultWhatsAppConfig().Window {
		t.Fatalf("rich config: %+v", h.richCfg)
	}
	// No notifications client and no Redis: nothing is sent and nothing fails.
	if err := h.signupReply(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	h.WithRich(creative.WhatsAppConfig{}, " https://mf.example/join ", time.Hour)
	if h.signupURL != "https://mf.example/join" || h.signupEvery != time.Hour || h.window() != creative.DefaultWhatsAppConfig().Window {
		t.Fatalf("signup config: %q %v", h.signupURL, h.signupEvery)
	}
}
