package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

func TestAssistantLookupAndAsk(t *testing.T) {
	var asked map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/s2s/users/by-phone":
			if r.Header.Get("X-API-Key") != "auth-key" {
				t.Errorf("auth key %q", r.Header.Get("X-API-Key"))
			}
			if r.URL.Query().Get("phone") == "+254700000000" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(`{"user_id":"` + uuid.NewString() + `","phone_verified":true,"tenants":[]}`))
		case "/api/v1/internal/os/whatsapp-ask":
			if r.Header.Get("X-API-Key") != "ai-key" {
				t.Errorf("ai key %q", r.Header.Get("X-API-Key"))
			}
			_ = json.NewDecoder(r.Body).Decode(&asked)
			_, _ = w.Write([]byte(`{"pass":true,"verdict":{"reason":"marketing request","source":"jev"},"reply":"Here is the plan"}`))
		}
	}))
	defer srv.Close()
	h := NewWhatsAppAssistantHandler(nil, nil, nil, srv.URL, "auth-key", srv.URL, "ai-key", "https://mf.example", "codevertex", zap.NewNop())
	ctx := context.Background()

	if u, err := h.lookup(ctx, "+254700000000"); err != nil || u != nil {
		t.Fatalf("an unknown phone is nobody: %v %v", u, err)
	}
	if u, err := h.lookup(ctx, "+254712345678"); err != nil || u == nil || u.UserID == "" {
		t.Fatalf("a known phone: %v %v", u, err)
	}
	a, err := h.ask(ctx, assistantTenant{ID: uuid.New(), Slug: "boi"}, "u1", "+254712345678", "make a video ad", true)
	if err != nil || !a.Pass || a.Reply != "Here is the plan" {
		t.Fatalf("ask: %+v %v", a, err)
	}
	// The verified number goes with the message: approval codes are bound to it.
	if asked["tenant_slug"] != "boi" || asked["session_active"] != true || asked["portal_url"] != "https://mf.example" || asked["sender"] != "+254712345678" {
		t.Fatalf("ask body: %v", asked)
	}
}

func TestAssistantEligibleTenantsNeedsAnApprovingRole(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db, "wa-assist")
	other := seedTenant(t, db, "wa-assist-viewer")
	authID := uuid.New()

	owner := db.MarketflowRole.Create().SetTenantID(tid).SetRoleCode("fbo_owner").SetName("Owner").SaveX(ctx)
	viewer := db.MarketflowRole.Create().SetTenantID(other).SetRoleCode("viewer").SetName("Viewer").SaveX(ctx)
	// One MarketFlow user per auth user (auth_service_user_id is unique); roles per tenant are
	// assignments.
	u := db.MarketflowUser.Create().SetTenantID(tid).SetAuthServiceUserID(authID).SetEmail("o@example.com").SaveX(ctx)
	db.UserRoleAssignment.Create().SetTenantID(tid).SetUserID(u.ID).SetRoleID(owner.ID).SetAssignedBy(authID).SaveX(ctx)
	db.UserRoleAssignment.Create().SetTenantID(other).SetUserID(u.ID).SetRoleID(viewer.ID).SetAssignedBy(authID).SaveX(ctx)

	h := NewWhatsAppAssistantHandler(db, nil, nil, "", "", "", "", "", "codevertex", zap.NewNop())
	pu := &phoneUser{UserID: authID.String()}
	for _, id := range []uuid.UUID{tid, other, uuid.New()} {
		pu.Tenants = append(pu.Tenants, struct {
			TenantID string   `json:"tenant_id"`
			Slug     string   `json:"slug"`
			Roles    []string `json:"roles"`
		}{TenantID: id.String(), Roles: []string{"fbo_owner"}})
	}
	got, err := h.eligibleTenants(ctx, pu)
	if err != nil {
		t.Fatal(err)
	}
	// auth-api says owner everywhere, but MarketFlow's own roles decide: only the first tenant.
	if len(got) != 1 || got[0].ID != tid {
		t.Fatalf("eligible: %+v", got)
	}
}

// A bare "YES <code>" from someone with several businesses is routed to the business that holds
// the code; marketflow-ai answers which, from the user, the number and the code.
func TestAssistantApprovalTenant(t *testing.T) {
	a, b := assistantTenant{ID: uuid.New(), Slug: "a"}, assistantTenant{ID: uuid.New(), Slug: "b"}
	var asked map[string]any
	answer := `{"yes":true,"tenant_id":"` + b.ID.String() + `"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/internal/os/approval-tenant" || r.Header.Get("X-API-Key") != "ai-key" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&asked)
		_, _ = w.Write([]byte(answer))
	}))
	defer srv.Close()
	h := NewWhatsAppAssistantHandler(nil, nil, nil, "", "", srv.URL, "ai-key", "", "codevertex", zap.NewNop())
	ctx := context.Background()

	found, yes, err := h.approvalTenant(ctx, "u1", "254712345678", "YES 4821", []assistantTenant{a, b})
	if err != nil || !yes || found == nil || found.ID != b.ID {
		t.Fatalf("found = %+v %v %v", found, yes, err)
	}
	if asked["sender"] != "+254712345678" || asked["user_id"] != "u1" || len(asked["tenant_ids"].([]any)) != 2 {
		t.Fatalf("lookup body: %v", asked)
	}
	// A YES whose code is pending nowhere: no business, but still a YES (the worker then asks).
	answer = `{"yes":true}`
	if found, yes, err := h.approvalTenant(ctx, "u1", "254712345678", "YES 4821", []assistantTenant{a, b}); err != nil || !yes || found != nil {
		t.Fatalf("not found = %+v %v %v", found, yes, err)
	}
}

func TestLooksLikeMarketing(t *testing.T) {
	for text, want := range map[string]bool{
		"Make a TikTok video for the sale": true,
		"nitengenezee tangazo":             true,
		"hello, is my invoice paid?":       false,
		"Adele concert tickets":            false, // "ad" only as a whole word
	} {
		if looksLikeMarketing(text) != want {
			t.Errorf("%q: want %v", text, want)
		}
	}
}
