package adsmanager

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestVerifiedPhone(t *testing.T) {
	user := uuid.New()
	other := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "k" {
			http.Error(w, "no key", http.StatusUnauthorized)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/users/"+user.String()+"/phone"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"user_id":"` + user.String() + `","phone":"+254712345678","phone_verified":true}`))
		case strings.HasSuffix(r.URL.Path, "/users/"+other.String()+"/phone"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"this user has no verified phone","code":"not_found"}`))
		default:
			http.NotFound(w, r) // an auth-api without the route
		}
	}))
	defer srv.Close()

	// rdb nil: the assistant fallback finds nothing, so a fallback reads as "no phone".
	phone := VerifiedPhone(nil, srv.URL, "k")
	if p, err := phone(context.Background(), uuid.New(), user); err != nil || p != "254712345678" {
		t.Fatalf("verified phone: %q, %v", p, err)
	}
	if p, err := phone(context.Background(), uuid.New(), other); err != nil || p != "" {
		t.Fatalf("no verified phone: %q, %v", p, err)
	}
	if p, err := phone(context.Background(), uuid.New(), uuid.New()); err != nil || p != "" {
		t.Fatalf("route missing falls back: %q, %v", p, err)
	}
}
