package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// Headers a service-key caller sends to say who it acts for. X-Acting-User-ID is the SSO user id
// of the signed-in person behind the call; X-Acting-Origin names an internal caller with no
// person behind it (only values in an explicit allowlist are honoured).
const (
	HeaderActingUserID = "X-Acting-User-ID"
	HeaderActingOrigin = "X-Acting-Origin"
)

// Acting is what a service caller declared about who it acts for.
type Acting struct {
	// UserID is the SSO user id from X-Acting-User-ID, uuid.Nil when absent.
	UserID uuid.UUID
	// Origin is X-Acting-Origin, lower-cased and trimmed.
	Origin string
	// Invalid is set when X-Acting-User-ID is present but not a uuid.
	Invalid bool
}

// ActingFrom reads X-Acting-User-ID and X-Acting-Origin. Both role gates use it.
func ActingFrom(r *http.Request) Acting {
	a := Acting{Origin: strings.ToLower(strings.TrimSpace(r.Header.Get(HeaderActingOrigin)))}
	raw := strings.TrimSpace(r.Header.Get(HeaderActingUserID))
	if raw == "" {
		return a
	}
	id, err := uuid.Parse(raw)
	if err != nil || id == uuid.Nil {
		a.Invalid = true
		return a
	}
	a.UserID = id
	return a
}

type actingUserKey struct{}

// WithActingUser records the SSO user a service caller acts for, once the role gate accepted it.
func WithActingUser(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, actingUserKey{}, id)
}

// ActingUser returns the SSO user a service caller acts for, set by the role gate.
func ActingUser(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(actingUserKey{}).(uuid.UUID)
	return id, ok && id != uuid.Nil
}
