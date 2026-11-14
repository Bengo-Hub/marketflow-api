package brandimages

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"math/bits"
	"strconv"

	"entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"golang.org/x/image/draw"

	enttenant "github.com/bengobox/marketflow-api/internal/ent/tenant"
)

// Duplicates (owner, 2026-11-14): the pictures a business uploads are often the same ones its
// website shows. Uploads are resized and re-encoded, so their bytes never match the website's
// file; a difference hash of the picture's shape does. A website picture whose hash is close to
// one the business already has is not linked again.

// dupDistance is the most bits two 64-bit difference hashes may differ by and still be the same
// picture (resizing and re-encoding move a few bits; different photos differ by 20 or more).
const dupDistance = 10

// dHash is a 64-bit difference hash: the picture shrunk to 9 x 8 grey pixels, one bit per pair of
// neighbours in a row (is the left one brighter). ok is false when the picture cannot be read.
func dHash(data []byte) (uint64, bool) {
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return 0, false
	}
	small := image.NewGray(image.Rect(0, 0, 9, 8))
	draw.ApproxBiLinear.Scale(small, small.Bounds(), src, src.Bounds(), draw.Src, nil)
	var h uint64
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			h <<= 1
			if small.GrayAt(x, y).Y > small.GrayAt(x+1, y).Y {
				h |= 1
			}
		}
	}
	return h, true
}

func formatHash(h uint64) string { return fmt.Sprintf("%016x", h) }

func parseHash(s string) (uint64, bool) {
	if len(s) != 16 {
		return 0, false
	}
	h, err := strconv.ParseUint(s, 16, 64)
	return h, err == nil
}

// nearAny reports whether h is within dupDistance of one of the hashes.
func nearAny(h uint64, hashes []uint64) bool {
	for _, o := range hashes {
		if bits.OnesCount64(h^o) <= dupDistance {
			return true
		}
	}
	return false
}

// existingHashes returns the hashes of a tenant's pictures: the recorded one, or for pictures
// saved before hashes were kept, the hash of the stored file (best effort).
func (s *Service) existingHashes(ctx context.Context, have []Image) []uint64 {
	var out []uint64
	for _, img := range have {
		if h, ok := parseHash(img.Hash); ok {
			out = append(out, h)
			continue
		}
		data, _, err := s.store.Get(ctx, img.Key)
		if err != nil {
			continue
		}
		if h, ok := dHash(data); ok {
			out = append(out, h)
		}
	}
	return out
}

// OrphanKey lists stored files whose delete failed; the daily sweep deletes them again, so a
// removed picture never stays in storage (owner, 2026-11-14: delete is a hard delete).
const OrphanKey = "brand_images_orphans"

// rememberOrphan records a file whose delete failed, for the sweep to retry.
func (s *Service) rememberOrphan(ctx context.Context, tenantID uuid.UUID, key string) {
	b, _ := json.Marshal([]string{key})
	col := enttenant.FieldMetadata
	list := "COALESCE(" + col + "->'" + OrphanKey + "', '[]'::jsonb)"
	_, err := s.db.Tenant.Update().Where(enttenant.ID(tenantID)).
		Modify(func(u *sql.UpdateBuilder) {
			u.Set(col, sql.ExprFunc(func(b2 *sql.Builder) {
				b2.WriteString("jsonb_set(COALESCE(" + col + ", '{}'::jsonb), '{" + OrphanKey + "}', " + list + " || ")
				b2.Arg(string(b))
				b2.WriteString("::jsonb)")
			}))
		}).Save(ctx)
	if err != nil {
		s.logger.Error("brand image orphan not recorded", zap.String("key", key), zap.Error(err))
	}
}

// retryOrphans deletes the recorded orphan files of every tenant again and forgets the ones that
// are gone (a file already missing counts as gone).
func (s *Service) retryOrphans(ctx context.Context) {
	tenants, err := s.db.Tenant.Query().All(ctx)
	if err != nil {
		return
	}
	for _, t := range tenants {
		raw, ok := t.Metadata[OrphanKey].([]any)
		if !ok || len(raw) == 0 {
			continue
		}
		var gone []string
		for _, v := range raw {
			key, _ := v.(string)
			if key == "" || !Owned(key, t.ID) {
				gone = append(gone, key) // never ours to delete: drop it from the list
				continue
			}
			if err := s.store.Delete(ctx, key); err == nil {
				gone = append(gone, key)
			}
		}
		if len(gone) == 0 {
			continue
		}
		// Remove only the keys handled here (jsonb "-" drops matching strings), so an orphan
		// recorded meanwhile stays on the list.
		col := enttenant.FieldMetadata
		_, _ = s.db.Tenant.Update().Where(enttenant.ID(t.ID)).
			Modify(func(u *sql.UpdateBuilder) {
				u.Set(col, sql.ExprFunc(func(b2 *sql.Builder) {
					b2.WriteString("jsonb_set(COALESCE(" + col + ", '{}'::jsonb), '{" + OrphanKey + "}', COALESCE(" + col + "->'" + OrphanKey + "', '[]'::jsonb)")
					for _, k := range gone {
						b2.WriteString(" - ")
						b2.Arg(k)
						b2.WriteString("::text")
					}
					b2.WriteString(")")
				}))
			}).Save(ctx)
	}
}
