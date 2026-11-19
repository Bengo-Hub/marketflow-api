package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Every group has members, no provider sits twice in one group, and the default is the saved
// member or else the first one (Tavily for web search).
func TestProviderGroupsHaveOneDefault(t *testing.T) {
	for _, g := range ProviderGroups {
		if len(g.Members) == 0 {
			t.Fatalf("%s has no members", g.ID)
		}
		seen := map[string]bool{}
		for _, m := range g.Members {
			if seen[m] {
				t.Fatalf("%s lists %s twice", g.ID, m)
			}
			seen[m] = true
		}
	}
	ws, _ := groupByID("web_search")
	if GroupDefault(ws, nil) != "tavily" {
		t.Fatal("Tavily is the web search default")
	}
	if GroupDefault(ws, map[string]string{"web_search": "vertex_search"}) != "vertex_search" {
		t.Fatal("a saved member is the default")
	}
	if GroupDefault(ws, map[string]string{"web_search": "custom_search"}) != "tavily" {
		t.Fatal("a provider no longer in the group falls back to the first member")
	}
	for _, g := range ProviderGroups {
		for _, m := range g.Members {
			if m == "custom_search" {
				t.Fatal("Google Custom Search is retired")
			}
		}
	}
}

// The balance watcher in marketflow-ai writes untagged Go field names; the cards get the
// remaining amount, its unit, the low mark, when it was read and whether it is low.
func TestDecodeBalances(t *testing.T) {
	raw := []byte(`{"fal":{"Remaining":4.49,"Unit":"usd","LowMark":5,"Estimate":false,"At":"2026-11-19T08:15:00Z"},
		"elevenlabs":{"Remaining":14707,"Unit":"characters","LowMark":10000,"At":"2026-11-19T08:15:00Z"}}`)
	b, err := decodeBalances(raw)
	if err != nil {
		t.Fatal(err)
	}
	if f := b["fal"]; f.Remaining != 4.49 || f.Unit != "usd" || !f.Low || f.At.IsZero() {
		t.Fatalf("fal = %+v", f)
	}
	if e := b["elevenlabs"]; e.Remaining != 14707 || e.Low {
		t.Fatalf("elevenlabs = %+v", e)
	}
	if out, err := readBalances(context.Background(), nil, "k"); err != nil || len(out) != 0 {
		t.Fatal("no Redis means no readings, not an error")
	}
}

func TestProbeTypeSafeAndVertex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/systemone":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if r.Header.Get("Authorization") != "Bearer ts-key" || body["model"] != "jev-latest" {
				http.Error(w, "no", http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"answers":{"ok":{"noul":0.9}}}`))
		case strings.HasSuffix(r.URL.Path, "/engines/web/servingConfigs/default_search:searchLite") && r.URL.Query().Get("key") == "AIza-k":
			_, _ = w.Write([]byte(`{"results":[]}`))
		default:
			http.Error(w, r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	if msg, ok := probeTypeSafe(ctx, srv.Client(), srv.URL, "ts-key", "jev-latest"); !ok {
		t.Fatalf("typesafe: %s", msg)
	}
	if _, ok := probeTypeSafe(ctx, srv.Client(), srv.URL, "bad", "jev-latest"); ok {
		t.Fatal("a bad TypeSafe key passed")
	}

	old := vertexSearchBase
	vertexSearchBase = func(string) string { return srv.URL }
	defer func() { vertexSearchBase = old }()
	if msg, ok := probeVertexSearch(ctx, srv.Client(), map[string]string{"project_id": "p", "engine_id": "web", "api_key": "AIza-k"}); !ok {
		t.Fatalf("vertex: %s", msg)
	}
	if _, ok := probeVertexSearch(ctx, srv.Client(), map[string]string{"project_id": "p", "engine_id": "web"}); ok {
		t.Fatal("no key and no service account passed")
	}
	sa := `{"client_email":"vera@p.iam.gserviceaccount.com","private_key":"placeholder-not-a-key"}`
	if msg, ok := probeVertexSearch(ctx, srv.Client(), map[string]string{"project_id": "p", "engine_id": "web", "service_account_json": sa}); !ok || !strings.Contains(msg, "vera@") {
		t.Fatalf("service account: %s %v", msg, ok)
	}
}
