package brandimages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	enttenant "github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/bengobox/marketflow-api/internal/modules/audit"
	"github.com/bengobox/marketflow-api/internal/modules/tenant/metastore"
	"github.com/bengobox/marketflow-api/internal/platform/svcconfig"
)

// Brand faces (owner approved 2026-11-24, plan item 66 f). A tenant admin marks one of the tenant's
// own brand images as a person who agreed to be the model for its AI presenter, and the agreement
// is kept as a consent record: who, what for, until when, who recorded it and when. Only a face
// with a live consent is ever sent to a model, only for this tenant. The feature is off until the
// tenant turns it on. A face is never found, inferred or copied from anywhere else, and one marked
// under 18 is refused. Ending a consent stops its use at once; the records stay as evidence
// without any picture. They live in Tenant.metadata (FacesKey, FacesOnKey), which only this package
// writes.
const (
	FacesKey   = "brand_faces"
	FacesOnKey = "brand_faces_on"
)

// BrandFacesKey is the platform's rules (service config, a platform row per tenant overrides):
// {"enabled":true,"max_faces":5,"max_days":365}. Off means no face is used for the tenants it covers.
const BrandFacesKey = "creative.brand_faces"

// FaceRules are the platform's limits.
type FaceRules struct {
	Enabled  bool `json:"enabled"`
	MaxFaces int  `json:"max_faces"`
	// MaxDays is the longest a consent may run before it is asked for again.
	MaxDays int `json:"max_days"`
}

// DefaultFaceRules are the fallbacks while the platform sets none.
func DefaultFaceRules() FaceRules { return FaceRules{Enabled: true, MaxFaces: 5, MaxDays: 365} }

// FaceRulesFor reads the rules in force for a tenant. An unreadable row turns faces off: a broken
// setting must not let a face through.
func FaceRulesFor(ctx context.Context, db *ent.Client, tenantID uuid.UUID) FaceRules {
	r := DefaultFaceRules()
	raw := svcconfig.Value(ctx, db, tenantID, BrandFacesKey)
	if raw == "" {
		return r
	}
	if json.Unmarshal([]byte(raw), &r) != nil {
		return FaceRules{}
	}
	if r.MaxFaces <= 0 || r.MaxFaces > 20 {
		r.MaxFaces = DefaultFaceRules().MaxFaces
	}
	if r.MaxDays <= 0 || r.MaxDays > 1095 {
		r.MaxDays = DefaultFaceRules().MaxDays
	}
	return r
}

// FaceUsePresenter is the one use a face can be recorded for today: the model for the AI presenter
// of the tenant's own video ads.
const FaceUsePresenter = "presenter"

// FaceStatementVersion names the statement the admin agrees to (FaceStatement); it is kept with
// each record so a later wording change never rewrites what was agreed.
const FaceStatementVersion = "2026-11-25"

// FaceStatement is what the admin confirms when recording a face.
const FaceStatement = "This person is an adult, is the only person in the picture, and agreed that this business may use the picture to make an AI presenter that looks like them in its video ads until the date given. They know the picture is sent to the AI image and video providers MarketFlow uses for this, that the videos say things they did not say themselves, and that they can withdraw at any time."

// Reasons a consent ended.
const (
	FaceRemoved = "removed"
	FaceExpired = "expired"
	FaceNoImage = "image_removed"
)

// Face is one consent record.
type Face struct {
	ID      string `json:"id"`
	ImageID string `json:"image_id"`
	Name    string `json:"name"`
	Use     string `json:"use"`
	// Until is the last moment the consent holds (RFC 3339).
	Until      string `json:"until"`
	Statement  string `json:"statement"`
	RecordedBy string `json:"recorded_by"`
	RecordedAt string `json:"recorded_at"`
	EndedAt    string `json:"ended_at,omitempty"`
	EndedBy    string `json:"ended_by,omitempty"`
	EndReason  string `json:"end_reason,omitempty"`
}

// Errors of recording a face.
var (
	ErrFacesOff     = errors.New("brand faces are off; turn them on first")
	ErrUnderage     = errors.New("a picture of someone under 18 is never used as a brand face")
	ErrFaceInvalid  = errors.New("invalid brand face")
	ErrFaceExists   = errors.New("this picture already has a live consent")
	ErrFaceNotFound = errors.New("brand face not found")
)

// FaceInput is the admin's record.
type FaceInput struct {
	ImageID string `json:"image_id"`
	Name    string `json:"name"`
	Use     string `json:"use"`
	// Until is a date, YYYY-MM-DD; the consent holds through that day.
	Until string `json:"until"`
	// Under18 must be answered; nil is refused, true is refused and nothing is stored.
	Under18 *bool `json:"under_18"`
	// Agreed confirms FaceStatement.
	Agreed bool   `json:"agreed"`
	By     string `json:"-"`
}

// ReadFaces returns the tenant's records, live and ended, oldest first.
func ReadFaces(meta map[string]any) []Face {
	b, err := json.Marshal(meta[FacesKey])
	if err != nil {
		return nil
	}
	var out []Face
	_ = json.Unmarshal(b, &out)
	return out
}

// FacesOn reports whether the tenant opted in.
func FacesOn(meta map[string]any) bool { on, _ := meta[FacesOnKey].(bool); return on }

// liveAt reports whether a record holds at a time; it needs a readable end date.
func (f Face) liveAt(now time.Time) bool {
	until, err := time.Parse(time.RFC3339, f.Until)
	return err == nil && f.ID != "" && f.EndedAt == "" && f.RecordedBy != "" && now.Before(until)
}

// LiveFace is a face that may be sent to a model now, with its picture.
type LiveFace struct {
	Face
	Key string
}

// LiveFaces are the faces that may be used now: the feature on for the platform and the tenant,
// the consent not ended or expired, and the picture still one of the tenant's own photos.
func LiveFaces(meta map[string]any, tenantID uuid.UUID, rules FaceRules, now time.Time) []LiveFace {
	if !rules.Enabled || !FacesOn(meta) {
		return nil
	}
	photos := map[string]Image{}
	for _, img := range OfKind(meta, tenantID, KindPhoto) {
		photos[img.ID] = img
	}
	var out []LiveFace
	for _, f := range ReadFaces(meta) {
		if img, ok := photos[f.ImageID]; ok && f.liveAt(now) && !LooksLikeLogo(img) {
			out = append(out, LiveFace{Face: f, Key: img.Key})
		}
	}
	return out
}

// LiveFaceByID is one live face, or false.
// ErrFaceUse is told to whoever would post or advertise a video whose person's consent ended. The
// posts already out stay up (owner, 2026-11-25).
var ErrFaceUse = errors.New("the consent of the person shown in this video has ended, so it can no longer be posted or advertised")

// VersionFace is the face a version's presenter was drawn from, "" for none.
func VersionFace(meta map[string]any) string {
	sb, _ := meta["storyboard"].(map[string]any)
	actor, _ := sb["actor"].(map[string]any)
	id, _ := actor["face_id"].(string)
	return id
}

// CheckUse refuses a new post or ad of a version whose person's consent is no longer live. A read
// that fails refuses too.
func CheckUse(ctx context.Context, db *ent.Client, tenantID, versionID uuid.UUID) error {
	v, err := db.CreativeVersion.Query().Where(creativeversion.ID(versionID), creativeversion.TenantID(tenantID)).Only(ctx)
	if err != nil {
		return err
	}
	id := VersionFace(v.Metadata)
	if id == "" {
		return nil
	}
	t, err := db.Tenant.Get(ctx, tenantID)
	if err != nil {
		return err
	}
	if _, ok := LiveFaceByID(t.Metadata, tenantID, FaceRulesFor(ctx, db, tenantID), time.Now(), id); !ok {
		return ErrFaceUse
	}
	return nil
}

func LiveFaceByID(meta map[string]any, tenantID uuid.UUID, rules FaceRules, now time.Time, id string) (LiveFace, bool) {
	for _, f := range LiveFaces(meta, tenantID, rules, now) {
		if id != "" && f.ID == id {
			return f, true
		}
	}
	return LiveFace{}, false
}

// FaceImages are the ids of pictures with a face record, live or ended. The consent covers the
// presenter only, so such a picture is never a poster reference or a scene frame.
func FaceImages(meta map[string]any) map[string]bool {
	out := map[string]bool{}
	for _, f := range ReadFaces(meta) {
		out[f.ImageID] = true
	}
	return out
}

// WithFaceEnded sets what runs when a consent ends (the purge of what was made from it).
func (s *Service) WithFaceEnded(fn func(ctx context.Context, tenantID uuid.UUID, faceID string) error) *Service {
	s.faceEnded = fn
	return s
}

// SetFacesOn records the tenant's opt-in or opt-out. Turning it off ends every live consent.
func (s *Service) SetFacesOn(ctx context.Context, tenantID uuid.UUID, on bool, by string) error {
	if on && !FaceRulesFor(ctx, s.db, tenantID).Enabled {
		return ErrFacesOff
	}
	if err := metastore.SetKeys(ctx, s.db, tenantID, map[string]any{FacesOnKey: on}); err != nil {
		return err
	}
	if on {
		return nil
	}
	t, err := s.db.Tenant.Get(ctx, tenantID)
	if err != nil {
		return err
	}
	var errs []error
	for _, f := range ReadFaces(t.Metadata) {
		if f.EndedAt == "" {
			errs = append(errs, s.EndFace(ctx, tenantID, f.ID, FaceRemoved, by))
		}
	}
	return errors.Join(errs...)
}

// AddFace records one person's consent. Nothing is stored when any part is missing.
func (s *Service) AddFace(ctx context.Context, tenantID uuid.UUID, in FaceInput) (Face, error) {
	rules := FaceRulesFor(ctx, s.db, tenantID)
	t, err := s.db.Tenant.Get(ctx, tenantID)
	if err != nil {
		return Face{}, err
	}
	if !rules.Enabled || !FacesOn(t.Metadata) {
		return Face{}, ErrFacesOff
	}
	if in.Under18 != nil && *in.Under18 {
		return Face{}, ErrUnderage
	}
	in.Name, in.By = strings.TrimSpace(in.Name), strings.TrimSpace(in.By)
	if in.Use == "" {
		in.Use = FaceUsePresenter
	}
	now := s.nowFn().UTC()
	day, derr := time.Parse("2006-01-02", strings.TrimSpace(in.Until))
	until := day.Add(24 * time.Hour)
	switch {
	case in.Under18 == nil:
		return Face{}, fmt.Errorf("%w: say whether the person is under 18", ErrFaceInvalid)
	case !in.Agreed:
		return Face{}, fmt.Errorf("%w: confirm that the person agreed", ErrFaceInvalid)
	case in.By == "" || in.By == "system":
		return Face{}, fmt.Errorf("%w: a signed-in person must record it", ErrFaceInvalid)
	case utf8.RuneCountInString(in.Name) < 2 || utf8.RuneCountInString(in.Name) > 80:
		return Face{}, fmt.Errorf("%w: give a name or label of 2 to 80 characters", ErrFaceInvalid)
	case in.Use != FaceUsePresenter:
		return Face{}, fmt.Errorf("%w: a face can be used for the presenter only", ErrFaceInvalid)
	case derr != nil || !until.After(now) || until.After(now.AddDate(0, 0, rules.MaxDays+1)):
		return Face{}, fmt.Errorf("%w: the consent must end on a date within the next %d days", ErrFaceInvalid, rules.MaxDays)
	}
	found := false
	for _, img := range OfKind(t.Metadata, tenantID, KindPhoto) {
		found = found || (img.ID == in.ImageID && !LooksLikeLogo(img))
	}
	if !found {
		return Face{}, ErrNotFound
	}
	live := 0
	for _, f := range ReadFaces(t.Metadata) {
		if f.liveAt(now) {
			live++
			if f.ImageID == in.ImageID {
				return Face{}, ErrFaceExists
			}
		}
	}
	if live >= rules.MaxFaces {
		return Face{}, ErrFull
	}
	f := Face{ID: uuid.NewString(), ImageID: in.ImageID, Name: in.Name, Use: in.Use, Until: until.Format(time.RFC3339),
		Statement: FaceStatementVersion, RecordedBy: in.By, RecordedAt: now.Format(time.RFC3339)}
	patch, err := json.Marshal([]Face{f})
	if err != nil {
		return Face{}, err
	}
	col := enttenant.FieldMetadata
	n, err := s.db.Tenant.Update().Where(enttenant.ID(tenantID)).
		Modify(func(u *sql.UpdateBuilder) {
			u.Set(col, sql.ExprFunc(func(b *sql.Builder) {
				b.WriteString("jsonb_set(COALESCE(" + col + ", '{}'::jsonb), '{" + FacesKey + "}', COALESCE(" + col + "->'" +
					FacesKey + "', '[]'::jsonb) || ")
				b.Arg(string(patch))
				b.WriteString("::jsonb)")
			}))
		}).Save(ctx)
	if err == nil && n == 0 {
		err = ErrNotFound
	}
	return f, err
}

// EndFace ends a live consent now and runs the purge of what was made from it. A record that has
// already ended is purged again, so a retry finishes a purge that failed.
func (s *Service) EndFace(ctx context.Context, tenantID uuid.UUID, id, reason, by string) error {
	t, err := s.db.Tenant.Get(ctx, tenantID)
	if err != nil {
		return err
	}
	var face *Face
	for _, f := range ReadFaces(t.Metadata) {
		if f.ID == id {
			face = &f
			break
		}
	}
	if face == nil {
		return ErrFaceNotFound
	}
	if face.EndedAt == "" {
		if err := s.markFaceEnded(ctx, tenantID, id, reason, by); err != nil {
			return err
		}
	}
	if s.faceEnded == nil {
		return nil
	}
	return s.faceEnded(ctx, tenantID, id)
}

// markFaceEnded stamps the end on a record in one UPDATE that keeps every other record.
func (s *Service) markFaceEnded(ctx context.Context, tenantID uuid.UUID, id, reason, by string) error {
	patch, err := json.Marshal(map[string]string{"ended_at": s.nowFn().UTC().Format(time.RFC3339), "ended_by": by, "end_reason": reason})
	if err != nil {
		return err
	}
	col := enttenant.FieldMetadata
	return s.db.Tenant.Update().Where(enttenant.ID(tenantID)).
		Modify(func(u *sql.UpdateBuilder) {
			u.Set(col, sql.ExprFunc(func(b *sql.Builder) {
				b.WriteString("jsonb_set(COALESCE(" + col + ", '{}'::jsonb), '{" + FacesKey +
					"}', (SELECT COALESCE(jsonb_agg(CASE WHEN e->>'id' = ")
				b.Arg(id)
				b.WriteString(" AND e->>'ended_at' IS NULL THEN e || ")
				b.Arg(string(patch))
				b.WriteString("::jsonb ELSE e END ORDER BY n), '[]'::jsonb) FROM jsonb_array_elements(COALESCE(" + col + "->'" +
					FacesKey + "', '[]'::jsonb)) WITH ORDINALITY AS t(e, n)))")
			}))
		}).Exec(ctx)
}

// endFacesOfImage ends the consents recorded on a picture that was removed or is no longer a photo.
func (s *Service) endFacesOfImage(ctx context.Context, tenantID uuid.UUID, imageID, by string) error {
	t, err := s.db.Tenant.Get(ctx, tenantID)
	if err != nil {
		return err
	}
	var errs []error
	for _, f := range ReadFaces(t.Metadata) {
		if f.ImageID == imageID && f.EndedAt == "" {
			errs = append(errs, s.EndFace(ctx, tenantID, f.ID, FaceNoImage, by))
		}
	}
	return errors.Join(errs...)
}

// endFacesAfter is endFacesOfImage after the picture has changed: a live face needs its picture, so
// use has already stopped, and a purge that fails here is repeated by the hourly sweep.
func (s *Service) endFacesAfter(ctx context.Context, tenantID uuid.UUID, imageID string) {
	if err := s.endFacesOfImage(context.WithoutCancel(ctx), tenantID, imageID, audit.Actor(ctx)); err != nil {
		s.logger.Warn("brand face of a removed picture not fully purged; the sweep retries", zap.Error(err))
	}
}

// SweepFaces ends what has expired or lost its picture, and repeats the purge of every ended
// consent: a video that was being made when the consent ended settles later and is caught here.
// A live consent's own tenant is the only one ever read or changed.
func (s *Service) SweepFaces(ctx context.Context) (ended int, err error) {
	tenants, err := s.db.Tenant.Query().Where(func(sel *sql.Selector) {
		sel.Where(sql.P(func(b *sql.Builder) {
			b.WriteString("jsonb_typeof(" + sel.C(enttenant.FieldMetadata) + "->'" + FacesKey + "') = 'array'")
		}))
	}).All(ctx)
	if err != nil {
		return 0, err
	}
	var errs []error
	for _, t := range tenants {
		n, err := s.sweepTenantFaces(ctx, t)
		ended += n
		errs = append(errs, err)
	}
	return ended, errors.Join(errs...)
}

func (s *Service) sweepTenantFaces(ctx context.Context, t *ent.Tenant) (ended int, err error) {
	now := s.nowFn().UTC()
	photos := map[string]bool{}
	for _, img := range OfKind(t.Metadata, t.ID, KindPhoto) {
		photos[img.ID] = true
	}
	var errs []error
	for _, f := range ReadFaces(t.Metadata) {
		reason := ""
		switch {
		case f.EndedAt != "":
			reason = f.EndReason
		case !f.liveAt(now):
			reason = FaceExpired
		case !photos[f.ImageID]:
			reason = FaceNoImage
		default:
			continue
		}
		if f.EndedAt == "" {
			ended++
		}
		errs = append(errs, s.EndFace(ctx, t.ID, f.ID, reason, "system"))
	}
	return ended, errors.Join(errs...)
}
