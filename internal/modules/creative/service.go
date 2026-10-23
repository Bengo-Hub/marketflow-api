// Package creative owns the Creative Studio data in marketflow-api (Sprint 15):
// briefs, the poster and video versions made from them, and their stored files.
// marketflow-ai makes the media (POST /api/v1/internal/creative/poster); the
// worker's Renderer calls it for queued versions and the Purger deletes draft
// files once they expire.
package creative

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/creativeasset"
	"github.com/bengobox/marketflow-api/internal/ent/creativebrief"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Version statuses.
const (
	StatusQueued     = "queued"
	StatusRendering  = "rendering"
	StatusReady      = "ready"
	StatusFailed     = "failed"
	StatusApproved   = "approved"
	StatusRejected   = "rejected"
	StatusSuperseded = "superseded"
)

// Limits.
const (
	// maxLiveDrafts caps a tenant's versions that are queued, rendering or
	// waiting for review, so a loop in a client cannot fill the render queue.
	maxLiveDrafts = 50
	// DraftTTL is how long an unapproved draft's files are kept.
	DraftTTL = 30 * 24 * time.Hour
	// rejectedTTL is how long a discarded draft's files are kept (undo window).
	rejectedTTL  = 24 * time.Hour
	maxPromptLen = 2000
	maxCopyLen   = 200
	maxPhotos    = 4
)

var (
	objectives = map[string]bool{"sales": true, "leads": true, "awareness": true, "traffic": true}
	languages  = map[string]bool{"en": true, "sw": true}
	formats    = map[string]bool{"1080x1080": true, "1080x1350": true, "1080x1920": true}
	tiers      = map[string]bool{"standard": true, "premium": true}
)

// ErrInvalid marks a request the caller must fix (400).
var ErrInvalid = errors.New("invalid")

// ErrConflict marks a change the version's status does not allow (409).
var ErrConflict = errors.New("conflict")

// ErrDraftLimit is returned when the tenant has too many live drafts (429).
var ErrDraftLimit = errors.New("too many drafts waiting")

func invalid(msg string) error { return fmt.Errorf("%w: %s", ErrInvalid, msg) }

// Service is the Creative Studio data layer.
type Service struct {
	db     *ent.Client
	logger *zap.Logger
	nowFn  func() time.Time
}

// NewService builds the service.
func NewService(db *ent.Client, logger *zap.Logger) *Service {
	return &Service{db: db, logger: logger.Named("creative"), nowFn: time.Now}
}

// BriefInput creates or changes a brief. Nil fields are left as they are.
type BriefInput struct {
	Title     *string        `json:"title"`
	Offering  map[string]any `json:"offering"`
	Objective *string        `json:"objective"`
	Audience  *string        `json:"audience"`
	Language  *string        `json:"language"`
	Status    *string        `json:"status"`
	Metadata  map[string]any `json:"metadata"`
}

func (in *BriefInput) validate(create bool) error {
	if in.Title != nil {
		t := strings.TrimSpace(*in.Title)
		if t == "" || len(t) > 160 {
			return invalid("title is required (max 160 characters)")
		}
		in.Title = &t
	} else if create {
		return invalid("title is required")
	}
	if in.Objective != nil && !objectives[*in.Objective] {
		return invalid("objective must be sales, leads, awareness or traffic")
	}
	if in.Language != nil && !languages[*in.Language] {
		return invalid("language must be en or sw")
	}
	if in.Audience != nil && len(*in.Audience) > 500 {
		return invalid("audience is limited to 500 characters")
	}
	if in.Status != nil && *in.Status != "active" && *in.Status != "archived" {
		return invalid("status must be active or archived")
	}
	return nil
}

// CreateBrief adds a brief.
func (s *Service) CreateBrief(ctx context.Context, tenantID uuid.UUID, createdBy *uuid.UUID, in BriefInput) (*ent.CreativeBrief, error) {
	if err := in.validate(true); err != nil {
		return nil, err
	}
	c := s.db.CreativeBrief.Create().
		SetTenantID(tenantID).
		SetTitle(*in.Title).
		SetNillableObjective(in.Objective).
		SetNillableAudience(in.Audience).
		SetNillableLanguage(in.Language).
		SetNillableCreatedBy(createdBy)
	if in.Offering != nil {
		c.SetOffering(in.Offering)
	}
	if in.Metadata != nil {
		c.SetMetadata(in.Metadata)
	}
	return c.Save(ctx)
}

// UpdateBrief changes the given fields of a brief.
func (s *Service) UpdateBrief(ctx context.Context, tenantID, id uuid.UUID, in BriefInput) (*ent.CreativeBrief, error) {
	if err := in.validate(false); err != nil {
		return nil, err
	}
	b, err := s.brief(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	u := b.Update()
	if in.Title != nil {
		u.SetTitle(*in.Title)
	}
	if in.Offering != nil {
		u.SetOffering(in.Offering)
	}
	if in.Objective != nil {
		u.SetObjective(*in.Objective)
	}
	if in.Audience != nil {
		u.SetAudience(*in.Audience)
	}
	if in.Language != nil {
		u.SetLanguage(*in.Language)
	}
	if in.Status != nil {
		u.SetStatus(*in.Status)
	}
	if in.Metadata != nil {
		u.SetMetadata(in.Metadata)
	}
	return u.Save(ctx)
}

// ListBriefs pages a tenant's briefs, newest first. An empty status means active.
func (s *Service) ListBriefs(ctx context.Context, tenantID uuid.UUID, status string, limit, offset int) ([]*ent.CreativeBrief, int, error) {
	if status == "" {
		status = "active"
	}
	q := s.db.CreativeBrief.Query().Where(creativebrief.TenantID(tenantID), creativebrief.Status(status))
	total, err := q.Clone().Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	rows, err := q.Order(ent.Desc(creativebrief.FieldCreatedAt)).Limit(limit).Offset(offset).All(ctx)
	return rows, total, err
}

// GetBrief returns a brief with its latest versions and their live files.
func (s *Service) GetBrief(ctx context.Context, tenantID, id uuid.UUID) (*ent.CreativeBrief, error) {
	return s.db.CreativeBrief.Query().
		Where(creativebrief.ID(id), creativebrief.TenantID(tenantID)).
		WithVersions(func(q *ent.CreativeVersionQuery) {
			q.Order(ent.Desc(creativeversion.FieldVersionNo)).Limit(50).
				WithAssets(func(a *ent.CreativeAssetQuery) { a.Where(creativeasset.PurgedAtIsNil()) })
		}).
		Only(ctx)
}

func (s *Service) brief(ctx context.Context, tenantID, id uuid.UUID) (*ent.CreativeBrief, error) {
	return s.db.CreativeBrief.Query().Where(creativebrief.ID(id), creativebrief.TenantID(tenantID)).Only(ctx)
}

// VersionInput asks for a new poster version of a brief.
type VersionInput struct {
	Prompt          string     `json:"prompt"`
	Format          string     `json:"format"`
	Tier            string     `json:"tier"`
	Headline        string     `json:"headline"`
	Subline         string     `json:"subline"`
	Price           string     `json:"price"`
	CTA             string     `json:"cta"`
	PhotoURLs       []string   `json:"photo_urls"`
	ParentVersionID *uuid.UUID `json:"parent_version_id"`
}

func (in *VersionInput) validate() error {
	in.Prompt = strings.TrimSpace(in.Prompt)
	if in.Prompt == "" || len(in.Prompt) > maxPromptLen {
		return invalid(fmt.Sprintf("prompt is required (max %d characters)", maxPromptLen))
	}
	if in.Format == "" {
		in.Format = "1080x1350"
	}
	if !formats[in.Format] {
		return invalid("format must be 1080x1080, 1080x1350 or 1080x1920")
	}
	if in.Tier == "" {
		in.Tier = "standard"
	}
	if !tiers[in.Tier] {
		return invalid("tier must be standard or premium")
	}
	for _, f := range []*string{&in.Headline, &in.Subline, &in.Price, &in.CTA} {
		*f = strings.TrimSpace(*f)
		if len(*f) > maxCopyLen {
			return invalid(fmt.Sprintf("poster text is limited to %d characters per line", maxCopyLen))
		}
	}
	if len(in.PhotoURLs) > maxPhotos {
		return invalid(fmt.Sprintf("at most %d photos", maxPhotos))
	}
	for _, u := range in.PhotoURLs {
		if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
			return invalid("photo_urls must be http(s) links")
		}
	}
	return nil
}

// CreateVersion queues a poster version; the worker's Renderer makes it.
func (s *Service) CreateVersion(ctx context.Context, tenantID, briefID uuid.UUID, in VersionInput) (*ent.CreativeVersion, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	if _, err := s.brief(ctx, tenantID, briefID); err != nil {
		return nil, err
	}
	if in.ParentVersionID != nil {
		ok, err := s.db.CreativeVersion.Query().
			Where(creativeversion.ID(*in.ParentVersionID), creativeversion.BriefID(briefID), creativeversion.TenantID(tenantID)).
			Exist(ctx)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, invalid("parent_version_id is not a version of this brief")
		}
	}
	live, err := s.db.CreativeVersion.Query().
		Where(creativeversion.TenantID(tenantID), creativeversion.StatusIn(StatusQueued, StatusRendering, StatusReady)).
		Count(ctx)
	if err != nil {
		return nil, err
	}
	if live >= maxLiveDrafts {
		return nil, ErrDraftLimit
	}
	copyFields := map[string]any{"prompt": in.Prompt, "headline": in.Headline, "subline": in.Subline, "price": in.Price, "cta": in.CTA}
	meta := map[string]any{}
	if len(in.PhotoURLs) > 0 {
		meta["photo_urls"] = in.PhotoURLs
	}
	// version_no is max+1; the unique (brief_id, version_no) index settles a race
	// between two requests, and the loser takes the next number.
	for attempt := 0; ; attempt++ {
		next := 1
		last, err := s.db.CreativeVersion.Query().
			Where(creativeversion.BriefID(briefID)).
			Order(ent.Desc(creativeversion.FieldVersionNo)).
			First(ctx)
		switch {
		case err == nil:
			next = last.VersionNo + 1
		case !ent.IsNotFound(err):
			return nil, err
		}
		v, err := s.db.CreativeVersion.Create().
			SetTenantID(tenantID).
			SetBriefID(briefID).
			SetNillableParentVersionID(in.ParentVersionID).
			SetVersionNo(next).
			SetKind("poster").
			SetTier(in.Tier).
			SetFormat(in.Format).
			SetCopy(copyFields).
			SetMetadata(meta).
			Save(ctx)
		if err == nil || !ent.IsConstraintError(err) || attempt >= 2 {
			return v, err
		}
	}
}

func (s *Service) version(ctx context.Context, tenantID, id uuid.UUID) (*ent.CreativeVersion, error) {
	return s.db.CreativeVersion.Query().
		Where(creativeversion.ID(id), creativeversion.TenantID(tenantID)).
		WithAssets(func(a *ent.CreativeAssetQuery) { a.Where(creativeasset.PurgedAtIsNil()) }).
		Only(ctx)
}

// GetVersion returns one version with its live files.
func (s *Service) GetVersion(ctx context.Context, tenantID, id uuid.UUID) (*ent.CreativeVersion, error) {
	return s.version(ctx, tenantID, id)
}

// Approve keeps a ready version: its files stop expiring.
func (s *Service) Approve(ctx context.Context, tenantID, id uuid.UUID, by *uuid.UUID) (*ent.CreativeVersion, error) {
	return s.decide(ctx, tenantID, id, StatusApproved, by, nil)
}

// Discard rejects a ready or failed version; its files go after a day.
func (s *Service) Discard(ctx context.Context, tenantID, id uuid.UUID) (*ent.CreativeVersion, error) {
	at := s.nowFn().Add(rejectedTTL)
	return s.decide(ctx, tenantID, id, StatusRejected, nil, &at)
}

// decide moves a version out of review in one transaction: the status change is
// conditional on the version still being in review, so two clicks agree.
func (s *Service) decide(ctx context.Context, tenantID, id uuid.UUID, status string, by *uuid.UUID, expires *time.Time) (*ent.CreativeVersion, error) {
	from := []string{StatusReady}
	if status == StatusRejected {
		from = append(from, StatusFailed)
	}
	tx, err := s.db.Tx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	u := tx.CreativeVersion.Update().
		Where(creativeversion.ID(id), creativeversion.TenantID(tenantID), creativeversion.StatusIn(from...)).
		SetStatus(status)
	if status == StatusApproved {
		u.SetApprovedAt(s.nowFn()).SetNillableApprovedBy(by)
	}
	n, err := u.Save(ctx)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		exists, err := tx.CreativeVersion.Query().Where(creativeversion.ID(id), creativeversion.TenantID(tenantID)).Exist(ctx)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, &ent.NotFoundError{}
		}
		return nil, fmt.Errorf("%w: only a version waiting for review can be %s", ErrConflict, status)
	}
	a := tx.CreativeAsset.Update().Where(creativeasset.VersionID(id), creativeasset.PurgedAtIsNil())
	if expires == nil {
		a.ClearExpiresAt()
	} else {
		a.SetExpiresAt(*expires)
	}
	if _, err := a.Save(ctx); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.version(ctx, tenantID, id)
}
