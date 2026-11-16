package httpapi

import (
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func TestActingFrom(t *testing.T) {
	id := uuid.New()
	cases := []struct {
		name, user, origin string
		wantUser           uuid.UUID
		wantOrigin         string
		wantInvalid        bool
	}{
		{"nothing", "", "", uuid.Nil, "", false},
		{"a user", id.String(), "", id, "", false},
		{"a user with spaces", "  " + id.String() + " ", "", id, "", false},
		{"not a uuid", "bob", "", uuid.Nil, "", true},
		{"the nil uuid", uuid.Nil.String(), "", uuid.Nil, "", true},
		{"an origin", "", " Worker ", uuid.Nil, "worker", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/", nil)
			if c.user != "" {
				r.Header.Set(HeaderActingUserID, c.user)
			}
			if c.origin != "" {
				r.Header.Set(HeaderActingOrigin, c.origin)
			}
			got := ActingFrom(r)
			if got.UserID != c.wantUser || got.Origin != c.wantOrigin || got.Invalid != c.wantInvalid {
				t.Fatalf("got %+v", got)
			}
		})
	}
	ctx := WithActingUser(t.Context(), id)
	if got, ok := ActingUser(ctx); !ok || got != id {
		t.Fatalf("ActingUser = %s %v", got, ok)
	}
	if _, ok := ActingUser(t.Context()); ok {
		t.Fatal("no acting user expected")
	}
}
