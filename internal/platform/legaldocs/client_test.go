package legaldocs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDocument_CachesAndKeepsLastKnown(t *testing.T) {
	var calls atomic.Int32
	var down atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if down.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if r.URL.Path != "/api/v1/legal/documents/MARKETFLOW_AUTOPUBLISH_TERMS" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"current":  map[string]any{"version": "v1", "effective_at": "2026-11-01T00:00:00Z"},
			"upcoming": map[string]any{"version": "v2", "effective_at": "2026-12-15T00:00:00Z"},
		})
	}))
	defer srv.Close()

	now := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	c := New(Config{BaseURL: srv.URL + "/", DocType: "MARKETFLOW_AUTOPUBLISH_TERMS", TTL: time.Minute, RetryAfter: 10 * time.Second})
	c.now = func() time.Time { return now }

	doc, err := c.Document(context.Background())
	if err != nil || !doc.Published || doc.Current.Version != "v1" || doc.Upcoming.Version != "v2" {
		t.Fatalf("doc = %+v, err = %v", doc, err)
	}
	if !doc.Upcoming.EffectiveAt.Equal(time.Date(2026, 12, 15, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("upcoming effective = %v", doc.Upcoming.EffectiveAt)
	}
	_, _ = c.Document(context.Background())
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1 (cached)", calls.Load())
	}

	// auth-api goes down after the TTL: the last known answer is kept.
	down.Store(true)
	now = now.Add(2 * time.Minute)
	doc, err = c.Document(context.Background())
	if err != nil || doc.Current == nil || doc.Current.Version != "v1" {
		t.Fatalf("last known not kept: %+v %v", doc, err)
	}
	// Within RetryAfter auth-api is not asked again.
	n := calls.Load()
	_, _ = c.Document(context.Background())
	if calls.Load() != n {
		t.Fatal("asked auth-api again inside RetryAfter")
	}
}

func TestDocument_UnknownWithoutLastKnown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := New(Config{BaseURL: srv.URL, DocType: "X"})
	if _, err := c.Document(context.Background()); err == nil {
		t.Fatal("want an error with nothing known")
	}
}

func TestDocument_NotPublished(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	doc, err := New(Config{BaseURL: srv.URL, DocType: "X"}).Document(context.Background())
	if err != nil || doc.Published {
		t.Fatalf("doc = %+v, err = %v, want definite not published", doc, err)
	}
}

func TestAcceptAndAccepted(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "k" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/api/v1/s2s/tenants/"+tenantID.String()+"/legal/acceptances" {
			t.Errorf("path = %s", r.URL.Path)
		}
		switch r.Method {
		case http.MethodGet:
			gets.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"acceptances": []map[string]any{{"version": "v1", "accepted_at": "2026-11-20T10:00:00Z"}}})
		case http.MethodPost:
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["doc_version"] == "old" {
				w.WriteHeader(http.StatusConflict)
				return
			}
			if body["user_id"] != userID.String() || body["ip_address"] != "203.0.113.9" {
				t.Errorf("body = %v", body)
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"version": body["doc_version"], "accepted_at": "2026-12-01T10:00:00Z", "accepted_by_user_id": userID.String()})
		}
	}))
	defer srv.Close()
	c := New(Config{BaseURL: srv.URL, APIKey: "k", DocType: "MARKETFLOW_AUTOPUBLISH_TERMS"})
	ctx := context.Background()

	list, err := c.Accepted(ctx, tenantID)
	if err != nil || len(list) != 1 || list[0].Version != "v1" {
		t.Fatalf("accepted = %+v, %v", list, err)
	}
	if _, err := c.Accept(ctx, tenantID, AcceptInput{Version: "old"}); !errors.Is(err, ErrNotAcceptable) {
		t.Fatalf("err = %v, want ErrNotAcceptable", err)
	}
	if _, err := c.Accept(ctx, tenantID, AcceptInput{Version: "v2", UserID: &userID, IP: "203.0.113.9"}); err != nil {
		t.Fatal(err)
	}
	list, _ = c.Accepted(ctx, tenantID)
	if len(list) != 2 || list[0].Version != "v2" || gets.Load() != 1 {
		t.Fatalf("cache not updated by Accept: %+v, gets = %d", list, gets.Load())
	}
}

func TestNew_DisabledWithoutURL(t *testing.T) {
	if New(Config{DocType: "X"}) != nil {
		t.Fatal("want nil without a base URL")
	}
}
