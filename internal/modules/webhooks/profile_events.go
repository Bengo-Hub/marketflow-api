package webhooks

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"go.uber.org/zap"
)

// ProfileEventRequest is the POST body for /api/v1/public/profile-events.
// Called by /p/{slug} at page view and on CTA clicks. No auth — same
// security posture as the lead-capture endpoint (rate-limited + IP-hashed).
type ProfileEventRequest struct {
	Slug      string         `json:"slug"`
	EventType string         `json:"event_type"`
	Referrer  string         `json:"referrer,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

var allowedProfileEvents = map[string]struct{}{
	"page_view":       {},
	"cta_click":       {},
	"whatsapp_click":  {},
	"chat_open":       {},
	"book_click":      {},
	"product_click":   {},
	"testimonial_view": {},
}

// RecordProfileEvent handles POST /api/v1/public/profile-events.
// @Summary Record a visitor interaction on /p/{slug}
// @Description Writes a `profile_events` row for dashboard visitor analytics. Unauthenticated; rate-limited upstream. IPs are SHA-256 hashed with a daily salt — no raw IPs or PII stored.
// @Tags Public
// @Accept json
// @Produce json
// @Param event body ProfileEventRequest true "Event payload"
// @Success 204
// @Failure 400 {object} httpapi.ErrorResponse
// @Router /public/profile-events [post]
func (h *PublicHandler) RecordProfileEvent(w http.ResponseWriter, r *http.Request) {
	var req ProfileEventRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
		return
	}
	req.Slug = strings.TrimSpace(strings.ToLower(req.Slug))
	req.EventType = strings.TrimSpace(strings.ToLower(req.EventType))
	if req.Slug == "" || req.EventType == "" {
		httpapi.Error(w, http.StatusBadRequest, "missing_fields", "slug and event_type are required", nil)
		return
	}
	if _, ok := allowedProfileEvents[req.EventType]; !ok {
		httpapi.Error(w, http.StatusBadRequest, "unknown_event_type", "unknown event_type", nil)
		return
	}

	t, err := h.entClient.Tenant.Query().
		Where(tenant.SlugEQ(req.Slug), tenant.StatusEQ("active")).
		Only(r.Context())
	if err != nil {
		// Return 204 so hostile traffic can't probe for valid slugs.
		w.WriteHeader(http.StatusNoContent)
		return
	}

	ipHash := hashClientIP(r.RemoteAddr, time.Now().UTC())
	ua := r.Header.Get("User-Agent")
	if len(ua) > 512 {
		ua = ua[:512]
	}
	ref := req.Referrer
	if len(ref) > 1024 {
		ref = ref[:1024]
	}

	builder := h.entClient.ProfileEvent.Create().
		SetTenantID(t.ID).
		SetSlug(req.Slug).
		SetEventType(req.EventType).
		SetIPHash(ipHash)
	if ua != "" {
		builder = builder.SetUserAgent(ua)
	}
	if ref != "" {
		builder = builder.SetReferrer(ref)
	}
	if req.Metadata != nil {
		builder = builder.SetMetadata(req.Metadata)
	}
	if _, err := builder.Save(r.Context()); err != nil {
		h.logger.Warn("profile_event save failed", zap.Error(err))
		// Still 204 — analytics failures must not break the page UX.
	}
	w.WriteHeader(http.StatusNoContent)
}

// hashClientIP returns a SHA-256 hex of the peer IP mixed with the UTC date
// so the same visitor produces the same hash on a given day (de-duping) but
// the hash cannot be correlated across days (reduces long-term tracking risk).
func hashClientIP(remoteAddr string, day time.Time) string {
	ip := remoteAddr
	if idx := strings.LastIndex(remoteAddr, ":"); idx != -1 {
		ip = remoteAddr[:idx]
	}
	salt := day.Format("2006-01-02")
	sum := sha256.Sum256([]byte(ip + "|" + salt))
	return hex.EncodeToString(sum[:])
}
