package creative

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/httpapi"
)

// A person may set only the allowlisted metadata keys; a service key acting for no person (make-
// media) sets system keys; a service key acting for a person is treated like the person; and a
// person's update merges instead of replacing, so system keys are never forged or wiped.
func TestBriefMetadataGuard(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	tid := tenantRow(t, db, map[string]any{})
	s := NewService(db, zap.NewNop())
	h := NewHandler(s, nil, nil, zap.NewNop())
	r := chi.NewRouter()
	r.Post("/creative/briefs", h.CreateBrief)
	r.Put("/creative/briefs/{id}", h.UpdateBrief)

	person := &authclient.Claims{TenantID: tid.String()}
	person.Subject = uuid.NewString() // promoted from the embedded registered claims
	service := &authclient.Claims{IsService: true, ServiceName: "marketflow-ai", TenantID: tid.String()}
	call := func(method, path, body string, claims *authclient.Claims, actingUser string) map[string]any {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("X-Tenant-ID", tid.String())
		req.Header.Set("Content-Type", "application/json")
		if actingUser != "" {
			req.Header.Set(httpapi.HeaderActingUserID, actingUser)
		}
		req = req.WithContext(authclient.ContextWithClaims(req.Context(), claims))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body)
		}
		var b struct {
			ID       string         `json:"id"`
			Metadata map[string]any `json:"metadata"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
			t.Fatal(err)
		}
		if b.Metadata == nil {
			b.Metadata = map[string]any{}
		}
		b.Metadata["_id"] = b.ID
		return b.Metadata
	}
	system := `"source":"os_turn","channel":"whatsapp","run_id":"run-1","requested_by":"` + uuid.NewString() +
		`","autopilot_agent_id":"` + uuid.NewString() + `","brand":{"accent":"#000000"}`
	plan := `"plan":{"option":{"label":"Test 2","credits":4}}`
	forged := []string{"source", "channel", "run_id", "requested_by", "autopilot_agent_id", "brand"}

	// A person: only the plan is kept.
	got := call(http.MethodPost, "/creative/briefs", `{"title":"Mine","metadata":{`+plan+`,`+system+`}}`, person, "")
	if _, ok := got["plan"]; !ok {
		t.Fatalf("the person's plan was dropped: %v", got)
	}
	for _, k := range forged {
		if _, ok := got[k]; ok {
			t.Errorf("a person set %s: %v", k, got)
		}
	}

	// A service key acting for a person (the AI agent's tools): the same as the person.
	got = call(http.MethodPost, "/creative/briefs", `{"title":"Via tools","metadata":{`+system+`}}`, service, uuid.NewString())
	for _, k := range forged {
		if _, ok := got[k]; ok {
			t.Errorf("a service key acting for a person set %s: %v", k, got)
		}
	}

	// A service key acting for no person (make-media): the system keys stay.
	sys := call(http.MethodPost, "/creative/briefs", `{"title":"Turn video","metadata":{`+plan+`,`+system+`}}`, service, "")
	for _, k := range append(forged, "plan") {
		if _, ok := sys[k]; !ok {
			t.Errorf("make-media lost %s: %v", k, sys)
		}
	}

	// A person's update merges: the plan changes, the system keys stay, a forged key is dropped.
	id := sys["_id"].(string)
	upd := call(http.MethodPut, "/creative/briefs/"+id,
		`{"metadata":{"plan":{"option":{"credits":2}},"source":"forged","run_id":"run-2"}}`, person, "")
	if upd["source"] != "os_turn" || upd["run_id"] != "run-1" || upd["autopilot_agent_id"] != sys["autopilot_agent_id"] {
		t.Fatalf("a person's update changed system keys: %v", upd)
	}
	if p, _ := upd["plan"].(map[string]any); p == nil || p["option"].(map[string]any)["credits"] != float64(2) {
		t.Fatalf("the plan was not updated: %v", upd)
	}

	// A person's update with only forbidden keys leaves the metadata as it was.
	same := call(http.MethodPut, "/creative/briefs/"+id, `{"metadata":{"source":"forged"}}`, person, "")
	if same["source"] != "os_turn" {
		t.Fatalf("a forbidden-only update changed the metadata: %v", same)
	}

	// The autopilot worker calls the Service directly and keeps its keys (no HTTP, no guard).
	title := "Autopilot"
	agent := uuid.NewString()
	b, err := s.CreateBrief(ctx, tid, nil, BriefInput{Title: &title, Metadata: map[string]any{"autopilot_agent_id": agent}})
	if err != nil || b.Metadata["autopilot_agent_id"] != agent {
		t.Fatalf("the worker's brief lost its agent: %v %v", err, b.Metadata)
	}
}

// The rule on its own: who is a system caller, and what a person keeps.
func TestBriefMetadataRule(t *testing.T) {
	req := func(c *authclient.Claims, acting string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		if acting != "" {
			r.Header.Set(httpapi.HeaderActingUserID, acting)
		}
		if c != nil {
			r = r.WithContext(authclient.ContextWithClaims(r.Context(), c))
		}
		return r
	}
	if systemBriefCaller(req(nil, "")) {
		t.Error("no claims is not a system caller")
	}
	if systemBriefCaller(req(&authclient.Claims{IsPlatformOwner: true}, "")) {
		t.Error("a platform owner person is not a system caller")
	}
	if !systemBriefCaller(req(&authclient.Claims{IsService: true}, "")) {
		t.Error("a service key with no acting person is a system caller")
	}
	if systemBriefCaller(req(&authclient.Claims{IsService: true}, uuid.NewString())) {
		t.Error("a service key acting for a person is not a system caller")
	}
	if got := personBriefMetadata(map[string]any{"source": "x"}); got != nil {
		t.Errorf("nothing allowed must be nil: %v", got)
	}
	if got := personBriefMetadata(map[string]any{"plan": map[string]any{}, "run_id": "r"}); len(got) != 1 {
		t.Errorf("only the plan is kept: %v", got)
	}
}
