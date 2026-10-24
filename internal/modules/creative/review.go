package creative

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/creativeasset"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/google/uuid"
)

// A review link lets someone without a MarketFlow login see one draft:
// /r/{token}, where the token is the version id, an expiry, the version's
// review epoch and an HMAC over them. Revoking bumps the epoch, so every link
// made before stops working.

const (
	reviewDomain     = "marketflow/creative-review/v1"
	reviewMACLen     = 16
	reviewTokenLen   = 16 + 4 + 2 + reviewMACLen
	DefaultReviewTTL = 72 * time.Hour
	maxReviewTTL     = 7 * 24 * time.Hour
)

// ErrReviewLink is any link that is malformed, forged, expired, revoked or
// points at a draft that is gone. Callers answer all of them the same way.
var ErrReviewLink = errors.New("review link is not valid")

// Reviews signs and opens review links.
type Reviews struct {
	db    *ent.Client
	key   []byte
	nowFn func() time.Time
}

// NewReviews builds Reviews; key is the service's HMAC key.
func NewReviews(db *ent.Client, key []byte) *Reviews {
	return &Reviews{db: db, key: key, nowFn: time.Now}
}

// Link makes a token for a version that has a file to show.
func (r *Reviews) Link(ctx context.Context, tenantID, versionID uuid.UUID, ttl time.Duration) (string, time.Time, error) {
	if len(r.key) == 0 {
		return "", time.Time{}, errors.New("review links are not configured")
	}
	if ttl <= 0 {
		ttl = DefaultReviewTTL
	}
	if ttl > maxReviewTTL {
		ttl = maxReviewTTL
	}
	v, err := r.db.CreativeVersion.Query().
		Where(creativeversion.ID(versionID), creativeversion.TenantID(tenantID)).
		Only(ctx)
	if err != nil {
		return "", time.Time{}, err
	}
	if v.Status != StatusReady && v.Status != StatusApproved {
		return "", time.Time{}, ErrConflict
	}
	exp := r.nowFn().Add(ttl).Truncate(time.Second)
	return r.sign(versionID, exp, uint16(intOf(v.Metadata["review_epoch"]))), exp, nil
}

// Revoke ends every review link of a version made so far.
func (r *Reviews) Revoke(ctx context.Context, tenantID, versionID uuid.UUID) error {
	v, err := r.db.CreativeVersion.Query().
		Where(creativeversion.ID(versionID), creativeversion.TenantID(tenantID)).
		Only(ctx)
	if err != nil {
		return err
	}
	meta := copyMap(v.Metadata)
	meta["review_epoch"] = intOf(meta["review_epoch"]) + 1
	return r.db.CreativeVersion.UpdateOneID(v.ID).SetMetadata(meta).Exec(ctx)
}

// Open checks a token and returns the URL of the draft's image or clip.
func (r *Reviews) Open(ctx context.Context, token string) (string, error) {
	id, exp, epoch, err := r.parse(token)
	if err != nil || r.nowFn().After(exp) {
		return "", ErrReviewLink
	}
	v, err := r.db.CreativeVersion.Query().
		Where(creativeversion.ID(id), creativeversion.StatusIn(StatusReady, StatusApproved)).
		WithAssets(func(q *ent.CreativeAssetQuery) {
			q.Where(creativeasset.PurgedAtIsNil(), creativeasset.KindIn(AssetImage, AssetVideo)).Limit(1)
		}).
		Only(ctx)
	if err != nil || uint16(intOf(v.Metadata["review_epoch"])) != epoch || len(v.Edges.Assets) == 0 {
		return "", ErrReviewLink
	}
	return v.Edges.Assets[0].URL, nil
}

func (r *Reviews) sign(id uuid.UUID, exp time.Time, epoch uint16) string {
	buf := make([]byte, 0, reviewTokenLen)
	buf = append(buf, id[:]...)
	buf = binary.BigEndian.AppendUint32(buf, uint32(exp.Unix()))
	buf = binary.BigEndian.AppendUint16(buf, epoch)
	buf = append(buf, r.mac(buf)...)
	return base64.RawURLEncoding.EncodeToString(buf)
}

func (r *Reviews) parse(token string) (uuid.UUID, time.Time, uint16, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != reviewTokenLen || len(r.key) == 0 {
		return uuid.Nil, time.Time{}, 0, ErrReviewLink
	}
	body, sum := raw[:reviewTokenLen-reviewMACLen], raw[reviewTokenLen-reviewMACLen:]
	if !hmac.Equal(sum, r.mac(body)) {
		return uuid.Nil, time.Time{}, 0, ErrReviewLink
	}
	id, _ := uuid.FromBytes(body[:16])
	exp := time.Unix(int64(binary.BigEndian.Uint32(body[16:20])), 0)
	return id, exp, binary.BigEndian.Uint16(body[20:22]), nil
}

func (r *Reviews) mac(body []byte) []byte {
	h := hmac.New(sha256.New, r.key)
	h.Write([]byte(reviewDomain))
	h.Write(body)
	return h.Sum(nil)[:reviewMACLen]
}
