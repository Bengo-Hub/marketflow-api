package testimonials

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Bengo-Hub/httpware/objectstore"
	ratelimit "github.com/Bengo-Hub/shared-ratelimit"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	mfmiddleware "github.com/bengobox/marketflow-api/internal/middleware"
)

// Public testimonial link (owner, 2026-11-24): /t/{slug} on marketflow-ui posts here. A customer
// gives their name, a rating, their words, an optional photo or video and their consent to
// publish; nothing is shown until an admin approves it.

// MediaType is the stored kind for customers' testimonial photos and videos: private, served by
// signed link, images re-encoded (which strips location data).
const MediaType = "testimonial-media"

// maxMediaBytes caps one photo or video.
const maxMediaBytes = 40 << 20

func init() {
	objectstore.Register(objectstore.MediaType{
		Name:     MediaType,
		MaxBytes: maxMediaBytes,
		Allowed:  []string{"image/jpeg", "image/png", "image/webp", "video/mp4", "video/webm", "video/quicktime"},
		Image:    true,
	})
}

// SubmitCaps are the daily caps on public submissions (0 turns one off).
type SubmitCaps struct {
	PerTenantDay int
	PerIPDay     int
}

// DefaultSubmitCaps: a busy shop gets far fewer than 50 reviews a day, and one visitor rarely
// sends more than three.
var DefaultSubmitCaps = SubmitCaps{PerTenantDay: 50, PerIPDay: 3}

type quotaChecker interface {
	Check(ctx context.Context, scopeID, feature string, limit int) (*ratelimit.QuotaResult, error)
}

// SubmitGuard caps public testimonials per tenant and per visitor address a day, like the public
// booking guard. It fails open when Redis errors; the router's per-IP limit still applies.
type SubmitGuard struct {
	caps  SubmitCaps
	quota quotaChecker
}

func NewSubmitGuard(quota quotaChecker, caps SubmitCaps) *SubmitGuard {
	return &SubmitGuard{caps: caps, quota: quota}
}

var errSubmitLimit = errors.New("too many testimonials today")

func (g *SubmitGuard) allow(ctx context.Context, tenantID uuid.UUID, ip string) error {
	if g == nil || g.quota == nil {
		return nil
	}
	for _, c := range []struct {
		scope, feature string
		limit          int
	}{
		{"ip:" + ip, "testimonials_ip", g.caps.PerIPDay},
		{"tenant:" + tenantID.String(), "testimonials_tenant", g.caps.PerTenantDay},
	} {
		if c.limit <= 0 || (strings.HasPrefix(c.scope, "ip:") && ip == "") {
			continue
		}
		if res, err := g.quota.Check(ctx, c.scope, c.feature, c.limit); err == nil && res != nil && !res.Allowed {
			return errSubmitLimit
		}
	}
	return nil
}

// WithGuard sets the daily caps on public submissions.
func (h *Handler) WithGuard(g *SubmitGuard) *Handler {
	h.guard = g
	return h
}

// minFillTime is the shortest time a person takes to write a testimonial; a faster form is a bot.
const minFillTime = 3 * time.Second

// Public form limits.
const (
	minPublicBody = 10
	maxPhoneLen   = 32
)

// publicForm is the public form after validation.
type publicForm struct {
	name, role, body, email, phone string
	rating                         int
	consent                        bool
}

// readPublicForm checks the fields of the multipart form. It returns a code and message for a 400,
// or a code "spam" for a bot (answered as if accepted).
func readPublicForm(r *http.Request, now time.Time) (publicForm, string, string) {
	f := publicForm{
		name:  strings.TrimSpace(r.FormValue("name")),
		role:  strings.TrimSpace(r.FormValue("role")),
		body:  strings.TrimSpace(r.FormValue("text")),
		email: strings.ToLower(strings.TrimSpace(r.FormValue("email"))),
		phone: strings.TrimSpace(r.FormValue("phone")),
	}
	// Honeypot: a field people never see. Fill time: the page stamps when the form was opened.
	if strings.TrimSpace(r.FormValue("website")) != "" {
		return f, "spam", ""
	}
	if ms, err := strconv.ParseInt(r.FormValue("started_at"), 10, 64); err != nil || now.Sub(time.UnixMilli(ms)) < minFillTime {
		return f, "spam", ""
	}
	f.consent = r.FormValue("consent") == "true" || r.FormValue("consent") == "on"
	f.rating, _ = strconv.Atoi(r.FormValue("rating"))
	switch {
	case f.name == "" || utf8.RuneCountInString(f.name) > maxNameLen:
		return f, "invalid_name", "give your name (at most 120 characters)"
	case utf8.RuneCountInString(f.role) > maxRoleLen:
		return f, "invalid_role", "the description of you is longer than 120 characters"
	case utf8.RuneCountInString(f.body) < minPublicBody || utf8.RuneCountInString(f.body) > maxBodyLen:
		return f, "invalid_text", "write between 10 and 2000 characters"
	case f.rating < 1 || f.rating > 5:
		return f, "invalid_rating", "choose a rating from 1 to 5"
	case !f.consent:
		return f, "consent_required", "tick the box to let the business publish your testimonial"
	case len(f.phone) > maxPhoneLen:
		return f, "invalid_phone", "the phone number is too long"
	case f.email != "" && (len(f.email) > 254 || !strings.Contains(f.email, "@")):
		return f, "invalid_email", "the email address is not valid"
	}
	return f, "", ""
}

// clientIP is the visitor's address (the router's RealIP middleware sets RemoteAddr).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func hashIP(ip string) string {
	if ip == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("testimonial:" + ip))
	return hex.EncodeToString(sum[:8])
}

// PublicSubmit handles POST /api/v1/public/testimonials/{slug} (multipart/form-data): name, role,
// text, rating, consent, optional email and phone, optional file (one photo or video), and the
// started_at stamp and the hidden website field the spam check reads. The testimonial waits for
// review; the tenant's admins are told.
// @Summary Send a testimonial to a business (public)
// @Tags Public
// @Accept mpfd
// @Produce json
// @Param slug path string true "Tenant slug"
// @Success 201 {object} map[string]interface{}
// @Failure 400 {object} httpapi.ErrorResponse
// @Failure 429 {object} httpapi.ErrorResponse
// @Router /public/testimonials/{slug} [post]
func (h *Handler) PublicSubmit(w http.ResponseWriter, r *http.Request) {
	slug := strings.ToLower(chi.URLParam(r, "slug"))
	t, err := h.db.Tenant.Query().Where(tenant.SlugEQ(slug), tenant.StatusEQ("active")).Only(r.Context())
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "tenant_not_found", "no business for this link", nil)
		return
	}
	mfmiddleware.SetUsageTenant(r.Context(), t.ID.String())

	// The whole form is size capped; a file is optional (a testimonial without a photo or video).
	r.Body = http.MaxBytesReader(w, r.Body, maxMediaBytes+(1<<20))
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") || r.ParseMultipartForm(8<<20) != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "send the form as multipart/form-data, with a file of at most 40 MB", nil)
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()
	form, code, msg := readPublicForm(r, time.Now())
	if code == "spam" {
		// A bot gets the same answer as a person, so it learns nothing.
		httpapi.JSON(w, http.StatusCreated, map[string]any{"status": "received"})
		return
	}
	if code != "" {
		httpapi.Error(w, http.StatusBadRequest, code, msg, nil)
		return
	}
	ip := clientIP(r)
	if err := h.guard.allow(r.Context(), t.ID, ip); err != nil {
		httpapi.Error(w, http.StatusTooManyRequests, "too_many", "this business has received many testimonials today; please try again tomorrow", nil)
		return
	}
	// The file is checked by its real type (and scanned) only once the form itself is fine.
	up, err := readMedia(r)
	if err != nil {
		if errors.Is(err, objectstore.ErrScanUnavailable) {
			httpapi.Error(w, http.StatusServiceUnavailable, "media_unavailable", "photos and videos cannot be checked right now; send the testimonial without one or try later", nil)
			return
		}
		httpapi.Error(w, http.StatusBadRequest, "invalid_file", "send one photo (JPEG, PNG, WebP) or video (MP4, WebM, MOV) of at most 40 MB", nil)
		return
	}

	meta := map[string]any{
		"consent":    true,
		"consent_at": time.Now().UTC().Format(time.RFC3339),
		"ip_hash":    hashIP(ip),
	}
	if form.email != "" {
		meta["submitter_email"] = form.email
	}
	if form.phone != "" {
		meta["submitter_phone"] = form.phone
	}
	if up != nil {
		if h.store == nil || !h.store.Enabled() {
			httpapi.Error(w, http.StatusServiceUnavailable, "media_unavailable", "photos and videos cannot be received right now; send the testimonial without one", nil)
			return
		}
		key, err := h.store.NewKey(objectstore.Owner{TenantID: t.ID.String()}, MediaType, up.Ext)
		if err == nil {
			err = h.store.Put(r.Context(), key, up.Data, up.ContentType)
		}
		if err != nil {
			h.logger.Error("testimonial media not stored", zap.Error(err))
			httpapi.Error(w, http.StatusBadGateway, "media_failed", "the photo or video could not be saved; try again or send without it", nil)
			return
		}
		meta["media_key"] = key
		meta["media_kind"] = "photo"
		if strings.HasPrefix(up.ContentType, "video/") {
			meta["media_kind"] = "video"
		}
		meta["media_type"] = up.ContentType
	}

	saved, err := Submit(r.Context(), h.db, SubmitInput{
		TenantID: t.ID, Source: SourceForm, AuthorName: form.name, AuthorRole: form.role,
		Body: form.body, Rating: form.rating, Metadata: meta,
	})
	if errors.Is(err, ErrDuplicate) {
		h.dropMedia(r.Context(), meta)
		httpapi.JSON(w, http.StatusCreated, map[string]any{"status": "received"})
		return
	}
	if err != nil {
		h.dropMedia(r.Context(), meta)
		h.logger.Error("public testimonial not saved", zap.String("tenant", t.ID.String()), zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "save_failed", "your testimonial could not be saved; please try again", nil)
		return
	}
	h.logger.Info("testimonial received", zap.String("tenant", t.ID.String()), zap.String("id", saved.ID.String()))
	httpapi.JSON(w, http.StatusCreated, map[string]any{"status": "received"})
}

func (h *Handler) dropMedia(ctx context.Context, meta map[string]any) {
	if key, _ := meta["media_key"].(string); key != "" && h.store != nil {
		_ = h.store.Delete(ctx, key)
	}
}

// readMedia returns the form's optional file, validated against MediaType (nil without one).
func readMedia(r *http.Request) (*objectstore.Upload, error) {
	if r.MultipartForm == nil || len(r.MultipartForm.File["file"]) == 0 {
		return nil, nil
	}
	fh := r.MultipartForm.File["file"][0]
	if fh.Size > maxMediaBytes {
		return nil, errors.New("file too large")
	}
	f, err := fh.Open()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxMediaBytes+1))
	if err != nil || len(raw) > maxMediaBytes {
		return nil, errors.New("file too large")
	}
	up, err := objectstore.Prepare(raw, MediaType)
	if err != nil {
		return nil, err
	}
	up.FileName = fh.Filename
	return up, nil
}
