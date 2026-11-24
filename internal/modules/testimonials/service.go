package testimonials

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/testimonial"
	"github.com/bengobox/marketflow-api/internal/outbox"
)

// Review states and sources. A testimonial the tenant writes itself is approved; one a customer
// sends through the public link, or one found on the tenant's website, waits for an admin
// (tenant admin or platform admin). Only an approved testimonial can be published, and
// MarketFlow never changes a customer's or a website's words: they are stored and shown as given.
const (
	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusRejected = "rejected"

	SourceManual  = "manual"
	SourceForm    = "form"
	SourceWebsite = "website"
)

// Submitted event: a testimonial now waits for review (form or website). The worker tells the
// tenant's admins.
const (
	TestimonialSubmittedEvent = "submitted"
	TestimonialSubmittedTopic = TestimonialAggregate + "." + TestimonialSubmittedEvent
)

// ErrDuplicate means the tenant already has this testimonial (same author and words).
var ErrDuplicate = errors.New("testimonial already received")

// Fingerprint is the dedupe key of a testimonial: a hash of the author and the words with case,
// spacing and punctuation ignored, so the same review sent twice or found again on the website is
// stored once.
func Fingerprint(author, body string) string {
	norm := func(s string) string {
		var b strings.Builder
		space := false
		for _, r := range strings.ToLower(s) {
			switch {
			case unicode.IsLetter(r) || unicode.IsDigit(r):
				b.WriteRune(r)
				space = false
			case !space:
				b.WriteRune(' ')
				space = true
			}
		}
		return strings.TrimSpace(b.String())
	}
	sum := sha256.Sum256([]byte(norm(author) + "|" + norm(body)))
	return hex.EncodeToString(sum[:])
}

// BodyFingerprint ignores the author: a website quote that matches a stored testimonial's words
// is the same testimonial whoever it is credited to.
func BodyFingerprint(body string) string { return Fingerprint("", body) }

// SubmitInput is a testimonial that needs review.
type SubmitInput struct {
	TenantID   uuid.UUID
	Source     string // form | website
	AuthorName string
	AuthorRole string
	Body       string
	Rating     int // 0 when the source gives none
	AvatarURL  string
	Metadata   map[string]any
}

// Submit stores a testimonial as pending, unpublished, with its submitted event in the same
// transaction. A testimonial the tenant already has (by fingerprint, or for a website quote by
// its words alone) returns ErrDuplicate and the existing row.
func Submit(ctx context.Context, db *ent.Client, in SubmitInput) (*ent.Testimonial, error) {
	fp := Fingerprint(in.AuthorName, in.Body)
	if t, err := findDuplicate(ctx, db, in.TenantID, fp, in.Body, in.Source == SourceWebsite); err != nil || t != nil {
		if t != nil {
			return t, ErrDuplicate
		}
		return nil, err
	}
	meta := map[string]any{"body_fingerprint": BodyFingerprint(in.Body)}
	for k, v := range in.Metadata {
		meta[k] = v
	}
	tx, err := db.Tx(ctx)
	if err != nil {
		return nil, err
	}
	b := tx.Testimonial.Create().
		SetTenantID(in.TenantID).
		SetAuthorName(in.AuthorName).
		SetBody(in.Body).
		SetIsPublished(false).
		SetStatus(StatusPending).
		SetSource(in.Source).
		SetFingerprint(fp).
		SetMetadata(meta)
	if in.Rating >= 1 && in.Rating <= 5 {
		b.SetRating(in.Rating)
	}
	if in.AuthorRole != "" {
		b.SetAuthorRole(in.AuthorRole)
	}
	if in.AvatarURL != "" {
		b.SetAvatarURL(in.AvatarURL)
	}
	t, err := b.Save(ctx)
	if err != nil {
		_ = tx.Rollback()
		if ent.IsConstraintError(err) {
			if dup, ferr := findDuplicate(ctx, db, in.TenantID, fp, in.Body, false); ferr == nil && dup != nil {
				return dup, ErrDuplicate
			}
		}
		return nil, fmt.Errorf("save testimonial: %w", err)
	}
	if err := outbox.Enqueue(ctx, tx.OutboxEvent, submittedEvent(t)); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return t, nil
}

func findDuplicate(ctx context.Context, db *ent.Client, tenantID uuid.UUID, fp, body string, byBody bool) (*ent.Testimonial, error) {
	t, err := db.Testimonial.Query().Where(testimonial.TenantID(tenantID), testimonial.Fingerprint(fp)).First(ctx)
	if err == nil {
		return t, nil
	}
	if !ent.IsNotFound(err) {
		return nil, err
	}
	if !byBody {
		return nil, nil
	}
	// Rows written before fingerprints, or credited differently: compare the words.
	rows, err := db.Testimonial.Query().Where(testimonial.TenantID(tenantID)).Limit(500).All(ctx)
	if err != nil {
		return nil, err
	}
	want := BodyFingerprint(body)
	for _, r := range rows {
		if BodyFingerprint(r.Body) == want {
			return r, nil
		}
	}
	return nil, nil
}

func submittedEvent(t *ent.Testimonial) *eventslib.Event {
	return eventslib.NewEvent(TestimonialSubmittedEvent, TestimonialAggregate, t.ID, t.TenantID, map[string]any{
		"testimonial_id": t.ID.String(),
		"source":         t.Source,
		"rating":         t.Rating,
	})
}

// ErrNotReviewable: only a pending or rejected testimonial can be approved, only a pending or
// approved one rejected.
var ErrNotReviewable = errors.New("testimonial cannot change to that state")

// Review approves or rejects a testimonial. Approving with publish shows it at once (and starts
// the testimonial ad draft, through the published event); rejecting always unpublishes.
func Review(ctx context.Context, db *ent.Client, tenantID, id uuid.UUID, approve, publish bool, reviewer string) (*ent.Testimonial, error) {
	existing, err := db.Testimonial.Query().Where(testimonial.ID(id), testimonial.TenantID(tenantID)).Only(ctx)
	if err != nil {
		return nil, err
	}
	to := StatusRejected
	if approve {
		to = StatusApproved
	}
	if existing.Status == to && (!approve || existing.IsPublished == publish) {
		return existing, nil
	}
	meta := map[string]any{}
	for k, v := range existing.Metadata {
		meta[k] = v
	}
	meta["reviewed_at"] = time.Now().UTC().Format(time.RFC3339)
	if reviewer != "" {
		meta["reviewed_by"] = reviewer
	}
	tx, err := db.Tx(ctx)
	if err != nil {
		return nil, err
	}
	t, err := tx.Testimonial.UpdateOne(existing).SetStatus(to).SetIsPublished(approve && publish).SetMetadata(meta).Save(ctx)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if becamePublished(existing.IsPublished, t) {
		if err := outbox.Enqueue(ctx, tx.OutboxEvent, publishedEvent(t)); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
	}
	return t, tx.Commit()
}
