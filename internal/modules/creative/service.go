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

	ratelimit "github.com/Bengo-Hub/shared-ratelimit"
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
	// waiting for review (the free-tier guard in the sprint plan).
	maxLiveDrafts = 5
	// DraftTTL is how long an unapproved draft's files are kept.
	DraftTTL = 72 * time.Hour
	// supersededTTL keeps the other drafts of a brief for a day after one of
	// them is approved.
	supersededTTL = 24 * time.Hour
	// extendedTTL is a draft's lifetime after its one extension.
	extendedTTL = 7 * 24 * time.Hour
	// dailyRenderCap bounds paid renders per tenant per UTC day (text-only
	// revisions are free and not counted).
	dailyRenderCap = 20
	maxPromptLen   = 2000
	maxCopyLen     = 200
	maxPhotos      = 4
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

// ErrDailyCap is returned when the tenant has used today's paid renders (429).
var ErrDailyCap = errors.New("daily render limit reached")

func invalid(msg string) error { return fmt.Errorf("%w: %s", ErrInvalid, msg) }

// Asset kinds.
const (
	AssetImage = "image"
	// AssetScene is the poster's scene without text, kept for free text edits.
	AssetScene = "scene"
)

// Service is the Creative Studio data layer.
type Service struct {
	db     *ent.Client
	quota  *ratelimit.Quota // daily paid-render cap; nil turns it off
	logger *zap.Logger
	nowFn  func() time.Time
}

// NewService builds the service.
func NewService(db *ent.Client, logger *zap.Logger) *Service {
	return &Service{db: db, logger: logger.Named("creative"), nowFn: time.Now}
}

// WithQuota turns on the per-tenant daily cap on paid renders (shared ratelimit,
// Redis day buckets; it fails open when Redis is down).
func (s *Service) WithQuota(q *ratelimit.Quota) *Service {
	s.quota = q
	return s
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
	brief, err := s.brief(ctx, tenantID, briefID)
	if err != nil {
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
	if err := s.checkLiveDrafts(ctx, tenantID); err != nil {
		return nil, err
	}
	// Last check before the insert: it consumes one of today's paid renders.
	if s.quota != nil {
		if res, _ := s.quota.Check(ctx, tenantID.String(), "creative_renders", dailyRenderCap); res != nil && !res.Allowed {
			return nil, ErrDailyCap
		}
	}
	copyFields := map[string]any{"prompt": in.Prompt, "headline": in.Headline, "subline": in.Subline, "price": in.Price, "cta": in.CTA}
	meta := map[string]any{}
	if len(in.PhotoURLs) > 0 {
		meta["photo_urls"] = in.PhotoURLs
	}
	if cats := restrictedCategories(brief.Title, str(brief.Offering["name"]), in.Prompt, in.Headline, in.Subline, in.CTA); len(cats) > 0 {
		meta["restricted_categories"] = cats
	}
	return s.insertVersion(ctx, tenantID, briefID, in.ParentVersionID, in.Tier, in.Format, copyFields, meta)
}

// checkLiveDrafts refuses a new version when the tenant already has
// maxLiveDrafts queued, rendering or waiting for review.
func (s *Service) checkLiveDrafts(ctx context.Context, tenantID uuid.UUID) error {
	live, err := s.db.CreativeVersion.Query().
		Where(creativeversion.TenantID(tenantID), creativeversion.StatusIn(StatusQueued, StatusRendering, StatusReady)).
		Count(ctx)
	if err != nil {
		return err
	}
	if live >= maxLiveDrafts {
		return ErrDraftLimit
	}
	return nil
}

// insertVersion adds a queued version. version_no is max+1; the unique
// (brief_id, version_no) index settles a race between two requests, and the
// loser takes the next number.
func (s *Service) insertVersion(ctx context.Context, tenantID, briefID uuid.UUID, parent *uuid.UUID, tier, format string, copyFields, meta map[string]any) (*ent.CreativeVersion, error) {
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
			SetNillableParentVersionID(parent).
			SetVersionNo(next).
			SetKind("poster").
			SetTier(tier).
			SetFormat(format).
			SetCopy(copyFields).
			SetMetadata(meta).
			Save(ctx)
		if err == nil || !ent.IsConstraintError(err) || attempt >= 2 {
			return v, err
		}
	}
}

// ReviseInput changes a poster's words only. Nil fields keep the source's.
type ReviseInput struct {
	Headline *string `json:"headline"`
	Subline  *string `json:"subline"`
	Price    *string `json:"price"`
	CTA      *string `json:"cta"`
}

// Revise queues a text-only revision of a finished poster: the same scene with
// new words, re-composed by marketflow-ai with no generation and no charge. It
// needs the scene stored with the source poster.
func (s *Service) Revise(ctx context.Context, tenantID, sourceID uuid.UUID, in ReviseInput) (*ent.CreativeVersion, error) {
	src, err := s.version(ctx, tenantID, sourceID)
	if err != nil {
		return nil, err
	}
	if src.Status != StatusReady && src.Status != StatusApproved && src.Status != StatusSuperseded {
		return nil, fmt.Errorf("%w: only a finished poster can be revised", ErrConflict)
	}
	scene := str(src.Metadata["scene_url"])
	for _, a := range src.Edges.Assets {
		if a.Kind == AssetScene {
			scene = a.URL
		}
	}
	if scene == "" {
		return nil, fmt.Errorf("%w: this poster has no stored scene; make a new version instead", ErrConflict)
	}
	copyFields := copyMap(src.Copy)
	for k, v := range map[string]*string{"headline": in.Headline, "subline": in.Subline, "price": in.Price, "cta": in.CTA} {
		if v == nil {
			continue
		}
		t := strings.TrimSpace(*v)
		if len(t) > maxCopyLen {
			return nil, invalid(fmt.Sprintf("poster text is limited to %d characters per line", maxCopyLen))
		}
		copyFields[k] = t
	}
	if err := s.checkLiveDrafts(ctx, tenantID); err != nil {
		return nil, err
	}
	meta := map[string]any{"recompose": true, "scene_url": scene}
	if cats := restrictedCategories(str(src.Copy["prompt"]), str(copyFields["headline"]), str(copyFields["subline"]), str(copyFields["cta"])); len(cats) > 0 {
		meta["restricted_categories"] = cats
	}
	return s.insertVersion(ctx, tenantID, src.BriefID, &src.ID, src.Tier, src.Format, copyFields, meta)
}

// Extend keeps a draft waiting for review for 7 days from when it was made,
// once (the sprint plan's single extension).
func (s *Service) Extend(ctx context.Context, tenantID, id uuid.UUID) (*ent.CreativeVersion, error) {
	v, err := s.version(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if v.Status != StatusReady {
		return nil, fmt.Errorf("%w: only a draft waiting for review can be extended", ErrConflict)
	}
	if _, done := v.Metadata["extended_at"]; done {
		return nil, fmt.Errorf("%w: a draft can be extended once", ErrConflict)
	}
	until := v.CreatedAt.Add(extendedTTL)
	err = withTx(ctx, s.db, func(tx *ent.Tx) error {
		meta := copyMap(v.Metadata)
		meta["extended_at"] = s.nowFn().UTC().Format(time.RFC3339)
		if err := tx.CreativeVersion.UpdateOneID(v.ID).SetMetadata(meta).Exec(ctx); err != nil {
			return err
		}
		return tx.CreativeAsset.Update().
			Where(creativeasset.VersionID(v.ID), creativeasset.PurgedAtIsNil(), creativeasset.ExpiresAtNotNil()).
			SetExpiresAt(until).
			Exec(ctx)
	})
	if err != nil {
		return nil, err
	}
	return s.version(ctx, tenantID, id)
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

// Approve keeps a ready version: its files stop expiring, and the brief's other
// drafts waiting for review are superseded (their files go after a day).
func (s *Service) Approve(ctx context.Context, tenantID, id uuid.UUID, by *uuid.UUID) (*ent.CreativeVersion, error) {
	return s.decide(ctx, tenantID, id, StatusApproved, by)
}

// Discard rejects a ready or failed version; the next hourly purge deletes its files.
func (s *Service) Discard(ctx context.Context, tenantID, id uuid.UUID) (*ent.CreativeVersion, error) {
	return s.decide(ctx, tenantID, id, StatusRejected, nil)
}

// decide moves a version out of review in one transaction: the status change is
// conditional on the version still being in review, so two clicks agree.
func (s *Service) decide(ctx context.Context, tenantID, id uuid.UUID, status string, by *uuid.UUID) (*ent.CreativeVersion, error) {
	now := s.nowFn()
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
		u.SetApprovedAt(now).SetNillableApprovedBy(by)
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
	if status == StatusApproved {
		a.ClearExpiresAt()
	} else {
		a.SetExpiresAt(now)
	}
	if _, err := a.Save(ctx); err != nil {
		return nil, err
	}
	if status == StatusApproved {
		if err := recordApproval(ctx, tx, id, by, now); err != nil {
			return nil, err
		}
		if err := supersedeSiblings(ctx, tx, id, now.Add(supersededTTL)); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.version(ctx, tenantID, id)
}

// recordApproval keeps the approval evidence (gate G5): who approved, where,
// when, and the checksum of exactly the file they saw, so a later dispute can
// show which poster was approved.
func recordApproval(ctx context.Context, tx *ent.Tx, id uuid.UUID, by *uuid.UUID, at time.Time) error {
	v, err := tx.CreativeVersion.Get(ctx, id)
	if err != nil {
		return err
	}
	evidence := map[string]any{"channel": "portal", "at": at.UTC().Format(time.RFC3339)}
	if by != nil {
		evidence["user_id"] = by.String()
	}
	if img, err := tx.CreativeAsset.Query().
		Where(creativeasset.VersionID(id), creativeasset.KindEQ(AssetImage), creativeasset.PurgedAtIsNil()).
		First(ctx); err == nil {
		evidence["asset_id"], evidence["sha256"] = img.ID.String(), img.Sha256
	}
	meta := copyMap(v.Metadata)
	meta["approval"] = evidence
	return tx.CreativeVersion.UpdateOneID(id).SetMetadata(meta).Exec(ctx)
}

// supersedeSiblings retires the other drafts of the approved version's brief
// and brings their files' expiry forward to until (never later than it was).
func supersedeSiblings(ctx context.Context, tx *ent.Tx, approvedID uuid.UUID, until time.Time) error {
	v, err := tx.CreativeVersion.Get(ctx, approvedID)
	if err != nil {
		return err
	}
	siblings, err := tx.CreativeVersion.Query().
		Where(creativeversion.BriefID(v.BriefID), creativeversion.IDNEQ(approvedID), creativeversion.StatusEQ(StatusReady)).
		IDs(ctx)
	if err != nil || len(siblings) == 0 {
		return err
	}
	if err := tx.CreativeVersion.Update().
		Where(creativeversion.IDIn(siblings...), creativeversion.StatusEQ(StatusReady)).
		SetStatus(StatusSuperseded).
		Exec(ctx); err != nil {
		return err
	}
	return tx.CreativeAsset.Update().
		Where(creativeasset.VersionIDIn(siblings...), creativeasset.PurgedAtIsNil(),
			creativeasset.Or(creativeasset.ExpiresAtIsNil(), creativeasset.ExpiresAtGT(until))).
		SetExpiresAt(until).
		Exec(ctx)
}
