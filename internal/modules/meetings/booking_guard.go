package meetings

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/mail"
	"strings"

	ratelimit "github.com/Bengo-Hub/shared-ratelimit"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Booking abuse guard (OWASP review M2, 2026-11-18). A booking makes Google send a calendar invite
// from the tenant's own calendar to the address given, so the public booking page and the AI's
// book_meeting tool could be used to send invites, with text of the sender's choosing, to anyone.
// The guard checks the address and caps the invites a day per tenant, per invited address and per
// visitor address. The callers also keep visitor-written notes out of the invite.
//
// An emailed one-time code before booking would prove the visitor owns the address; it changes
// the booking page's steps, so it is a follow-up.

// BookingCaps are the daily caps; 0 turns one off (config MF_SECURITY_BOOKINGS_PER_*_DAY).
type BookingCaps struct {
	PerTenantDay int
	PerEmailDay  int
	PerIPDay     int
}

// quotaChecker is ratelimit.Quota's daily counter: Check takes one use when it is allowed and
// fails open when Redis errors.
type quotaChecker interface {
	Check(ctx context.Context, scopeID, feature string, limit int) (*ratelimit.QuotaResult, error)
}

// BookingGuard checks an outside invite before it is sent.
type BookingGuard struct {
	caps   BookingCaps
	quota  quotaChecker
	logger *zap.Logger
}

// NewBookingGuard builds the guard; a nil quota turns the caps off (the address check stays).
func NewBookingGuard(quota quotaChecker, caps BookingCaps, logger *zap.Logger) *BookingGuard {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &BookingGuard{caps: caps, quota: quota, logger: logger.Named("booking_guard")}
}

// ErrBookingLimit is a booking past one of the daily caps.
var ErrBookingLimit = errors.New("too many bookings today; try again tomorrow or contact the business directly")

// bookableTypes are the meeting types an outside booking may ask for. The type names the
// invite's title (gcal humanMeetingType), which would otherwise carry any text a visitor sent.
var bookableTypes = map[string]bool{"product_demo": true, "business_opportunity": true, "follow_up": true, "onboarding": true}

// BookableMeetingType is t when it is a known type, else product_demo.
func BookableMeetingType(t string) string {
	if bookableTypes[t] {
		return t
	}
	return "product_demo"
}

// maxEmailLen is the longest address SMTP allows.
const maxEmailLen = 254

// NormalizeAttendeeEmail returns the address lower-cased and trimmed, and whether it is a plain,
// valid address: no display name, no spaces or angle brackets, a domain with a dot, at most 254
// characters.
func NormalizeAttendeeEmail(raw string) (string, bool) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" || len(s) > maxEmailLen || strings.ContainsAny(s, " <>,;\"\r\n\t") {
		return "", false
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Name != "" || a.Address != s {
		return "", false
	}
	at := strings.LastIndexByte(s, '@')
	if at < 1 || at > 64 {
		return "", false
	}
	domain := s[at+1:]
	if !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") || strings.Contains(domain, "..") {
		return "", false
	}
	return s, true
}

// Allow takes one use of each cap that applies, most specific first: the visitor's address (when
// ip is set), the invited address, then the tenant. It returns ErrBookingLimit at the first cap
// that is full. The invited address is counted by its hash, so no address sits in a Redis key.
func (g *BookingGuard) Allow(ctx context.Context, tenantID uuid.UUID, email, ip string) error {
	if g == nil || g.quota == nil {
		return nil
	}
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	checks := []struct {
		on      bool
		scope   string
		feature string
		limit   int
	}{
		{ip != "", "ip:" + ip, "booking_invites_ip", g.caps.PerIPDay},
		{email != "", "email:" + hex.EncodeToString(sum[:]), "booking_invites_email", g.caps.PerEmailDay},
		{true, "tenant:" + tenantID.String(), "booking_invites_tenant", g.caps.PerTenantDay},
	}
	for _, c := range checks {
		if !c.on || c.limit <= 0 {
			continue
		}
		res, err := g.quota.Check(ctx, c.scope, c.feature, c.limit)
		if err != nil {
			g.logger.Warn("booking cap check failed; allowing", zap.String("cap", c.feature), zap.Error(err))
			continue
		}
		if res != nil && !res.Allowed {
			g.logger.Info("booking cap reached", zap.String("cap", c.feature), zap.String("tenant_id", tenantID.String()))
			return ErrBookingLimit
		}
	}
	return nil
}
