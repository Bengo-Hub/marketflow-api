package worker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

func TestRetentionDays(t *testing.T) {
	p, err := ParseRetention(`{"default_days":10,"plans":{"PROFESSIONAL":30,"STARTER":2}}`, 4, 60)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		plan     string
		override int
		want     int
	}{
		{"", 0, 10},
		{"professional", 0, 30},   // plan codes match case-insensitively
		{"PROFESSIONAL", 45, 45},  // the tenant's own override wins
		{"STARTER", 0, 4},         // held at the minimum
		{"PROFESSIONAL", 400, 60}, // held at the maximum
	}
	for _, c := range cases {
		if got := p.Days(c.plan, c.override); got != c.want {
			t.Fatalf("Days(%q, %d) = %d, want %d", c.plan, c.override, got, c.want)
		}
	}
	if p, err := ParseRetention("{bad", 4, 60); err == nil || p.DefaultDays != 10 {
		t.Fatalf("unreadable JSON falls back to 10 days with an error: %+v %v", p, err)
	}
}

func TestDesiredRulesPerTenantAndCapped(t *testing.T) {
	p, _ := ParseRetention(`{"default_days":10,"plans":{"PRO":20}}`, 4, 90)
	big, small := uuid.New(), uuid.New()
	tenants := []TenantMedia{{ID: small, LiveBytes: 10}, {ID: big, Plan: "PRO", LiveBytes: 1000}}

	rules, dropped := DesiredRules(retentionTypes["public"], tenants, p, 0)
	if len(rules) != 6 || dropped != 0 {
		t.Fatalf("three public types per tenant: %d rules, %d dropped", len(rules), dropped)
	}
	for _, r := range rules {
		if !strings.HasPrefix(r.ID, retentionRulePrefix) || !strings.HasPrefix(r.Conditions["prefix"], "marketflow/") || !strings.HasSuffix(r.Conditions["prefix"], "/") {
			t.Fatalf("rule shape: %+v", r)
		}
		want := int64(10 * 86400)
		if strings.Contains(r.ID, big.String()) {
			want = 20 * 86400
		}
		if r.Delete.Condition.MaxAge != want || r.Delete.Condition.Type != "Age" {
			t.Fatalf("age for %s: %+v", r.ID, r.Delete.Condition)
		}
	}
	priv, _ := DesiredRules(retentionTypes["private"], tenants, p, 0)
	if len(priv) != 2 || priv[0].Conditions["prefix"] == "" || !strings.Contains(priv[0].Conditions["prefix"], "/video-sources/") {
		t.Fatalf("one private type per tenant: %+v", priv)
	}
	capped, dropped := DesiredRules(retentionTypes["public"], tenants, p, 3)
	if len(capped) != 3 || dropped != 1 || !strings.Contains(capped[0].ID, big.String()) {
		t.Fatalf("the cap keeps the tenant with the most live bytes: %d rules, %d dropped, %+v", len(capped), dropped, capped)
	}
}

// fakeR2 is Cloudflare's lifecycle API for two buckets, counting PUTs.
type fakeR2 struct {
	mu    sync.Mutex
	rules map[string][]json.RawMessage
	puts  int
}

func (f *fakeR2) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		parts := strings.Split(r.URL.Path, "/")
		bucket := parts[len(parts)-2]
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"rules": f.rules[bucket]}})
		case http.MethodPut:
			f.puts++
			var body struct {
				Rules []json.RawMessage `json:"rules"`
			}
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			f.rules[bucket] = body.Rules
			_, _ = w.Write([]byte(`{"success":true}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

type fakeUsage struct{ tenants []TenantMedia }

func (f fakeUsage) Tenants(context.Context) ([]TenantMedia, error) { return f.tenants, nil }

type fakeAlert struct{ subjects, texts []string }

func (f *fakeAlert) Send(_ context.Context, subject, text, _, _ string) {
	f.subjects = append(f.subjects, subject)
	f.texts = append(f.texts, text)
}

func TestMediaLifecycleSyncKeepsOtherRulesAndIsIdempotent(t *testing.T) {
	abort := json.RawMessage(`{"id":"abort-incomplete-multipart-1d","enabled":true,"conditions":{"prefix":""},"abortMultipartUploadsTransition":{"condition":{"type":"Age","maxAge":86400}}}`)
	stale := json.RawMessage(`{"id":"mf-ttl-gone-tenant-video-drafts","enabled":true,"conditions":{"prefix":"marketflow/gone/video-drafts/"}}`)
	r2 := &fakeR2{rules: map[string][]json.RawMessage{"pub": {abort, stale}, "priv": {abort}}}
	srv := r2.server(t)
	tid := uuid.New()
	cfg := MediaStorageConfig{R2AccountID: "acct", R2LifecycleToken: "tok", R2APIBase: srv.URL, Retention: `{"default_days":10}`, MinDays: 4, MaxDays: 90, MaxRules: 900}
	m := NewMediaLifecycle(cfg, "pub", "priv", fakeUsage{[]TenantMedia{{ID: tid, LiveBytes: 5}}}, &fakeAlert{}, nil, zap.NewNop())

	if err := m.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	pub := string(mustJSON(t, r2.rules["pub"]))
	if !strings.Contains(pub, "abort-incomplete-multipart-1d") {
		t.Fatalf("the bucket's own rule is kept: %s", pub)
	}
	if strings.Contains(pub, "gone-tenant") {
		t.Fatalf("a rule for a tenant with no live files is removed: %s", pub)
	}
	if !strings.Contains(pub, "marketflow/"+tid.String()+"/creative-drafts/") || !strings.Contains(pub, `"maxAge":864000`) {
		t.Fatalf("the tenant's drafts expire after 10 days: %s", pub)
	}
	if !strings.Contains(string(mustJSON(t, r2.rules["priv"])), "/video-sources/") {
		t.Fatal("the private bucket gets the video sources rule")
	}
	puts := r2.puts
	if err := m.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r2.puts != puts {
		t.Fatalf("nothing changed, so nothing is written: %d puts after %d", r2.puts, puts)
	}
}

// video-drafts holds finished videos too, so the sync leaves it alone unless switched on, and
// removes a video-drafts rule it made earlier.
func TestMediaLifecycleVideoDraftsOnlyWhenSwitchedOn(t *testing.T) {
	tid := uuid.New()
	earlier := json.RawMessage(`{"id":"mf-ttl-` + tid.String() + `-video-drafts","enabled":true,"conditions":{"prefix":"marketflow/` + tid.String() + `/video-drafts/"},"deleteObjectsTransition":{"condition":{"type":"Age","maxAge":864000}}}`)
	run := func(on bool) string {
		r2 := &fakeR2{rules: map[string][]json.RawMessage{"pub": {earlier}, "priv": {}}}
		srv := r2.server(t)
		cfg := MediaStorageConfig{R2AccountID: "acct", R2LifecycleToken: "tok", R2APIBase: srv.URL, MaxRules: 900, LifecycleVideoDrafts: on}
		m := NewMediaLifecycle(cfg, "pub", "priv", fakeUsage{[]TenantMedia{{ID: tid, LiveBytes: 5}}}, nil, nil, zap.NewNop())
		if err := m.Once(context.Background()); err != nil {
			t.Fatal(err)
		}
		return string(mustJSON(t, r2.rules["pub"]))
	}
	off := run(false)
	if strings.Contains(off, "/video-drafts/") {
		t.Fatalf("video-drafts is not managed by default, and the earlier rule is removed: %s", off)
	}
	if !strings.Contains(off, "/creative-drafts/") || !strings.Contains(off, "/audio-drafts/") {
		t.Fatalf("the other draft types are still managed: %s", off)
	}
	if on := run(true); !strings.Contains(on, "/video-drafts/") {
		t.Fatalf("MF_MEDIA_LIFECYCLE_VIDEO_DRAFTS=true manages video-drafts: %s", on)
	}
	if got := managedTypes(false)["public"]; len(got) != 2 {
		t.Fatalf("two public types without the switch, got %v", got)
	}
}

func TestMediaLifecycleAlertsAboveLiveBytes(t *testing.T) {
	alert := &fakeAlert{}
	big := uuid.New()
	usage := fakeUsage{[]TenantMedia{{ID: big, LiveBytes: 6 << 30}, {ID: uuid.New(), LiveBytes: 3 << 30}}}
	m := NewMediaLifecycle(MediaStorageConfig{LiveBytesAlert: 8 << 30, MaxRules: 900}, "", "", usage, alert, nil, zap.NewNop())
	if err := m.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(alert.subjects) != 1 || !strings.Contains(alert.subjects[0], "8.00 GiB") || !strings.Contains(alert.texts[0], big.String()) {
		t.Fatalf("one alert naming the level and the largest tenant: %v %v", alert.subjects, alert.texts)
	}
	quiet := &fakeAlert{}
	under := NewMediaLifecycle(MediaStorageConfig{LiveBytesAlert: 8 << 30}, "", "", fakeUsage{[]TenantMedia{{ID: big, LiveBytes: 1 << 30}}}, quiet, nil, zap.NewNop())
	_ = under.Once(context.Background())
	if len(quiet.subjects) != 0 {
		t.Fatal("below the level, no alert")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
