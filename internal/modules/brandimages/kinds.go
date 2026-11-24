package brandimages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"entgo.io/ent/dialect/sql"
	"github.com/Bengo-Hub/httpware/objectstore"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	enttenant "github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/modules/audit"
)

// Image kinds (plan item 66 e). A photo shows the business; a screenshot is one of its own app's
// screens, placed on a device's screen in a video scene (marketflow-ai media/screens.go). A
// screenshot is never a video frame, a poster reference or a brand photo, and the two kinds have
// their own limits.
const (
	KindPhoto      = ""
	KindScreenshot = "screenshot"
)

// Devices are the devices a screenshot can be shown on; "" goes by the picture's shape.
var Devices = map[string]bool{"": true, "laptop": true, "tablet": true, "pos": true, "phone": true}

// Errors of marking a picture as a screenshot.
var (
	// ErrCustomerData is a screenshot added without the confirmation that it shows no customer's data.
	ErrCustomerData = errors.New("confirm that the screenshot shows no customer's data")
	// ErrInvalidKind is an unknown kind or device.
	ErrInvalidKind = errors.New("unknown picture kind or device")
)

// NeverFrame reports a picture that is never a scene frame or a reference photo: a logo or a
// screenshot.
func NeverFrame(img Image) bool { return img.Kind == KindScreenshot || LooksLikeLogo(img) }

// OfKind are a tenant's own pictures of one kind, oldest first.
func OfKind(meta map[string]any, tenantID uuid.UUID, kind string) []Image {
	var out []Image
	for _, img := range Read(meta) {
		if img.Kind == kind && Owned(img.Key, tenantID) {
			out = append(out, img)
		}
	}
	return out
}

// KindUpdate changes a picture's kind.
type KindUpdate struct {
	Kind   string `json:"kind"`
	Device string `json:"device"`
	// NoCustomerData is the person's statement that the screenshot shows no customer's names,
	// phone numbers, payments or other data. Required for a screenshot and kept with it.
	NoCustomerData bool `json:"no_customer_data"`
	// By is who says so; set by the handler, never by the request.
	By string `json:"-"`
}

func (u *KindUpdate) check() error {
	u.Kind, u.Device = strings.TrimSpace(u.Kind), strings.ToLower(strings.TrimSpace(u.Device))
	if u.Kind == "photo" {
		u.Kind = KindPhoto
	}
	if (u.Kind != KindPhoto && u.Kind != KindScreenshot) || !Devices[u.Device] {
		return ErrInvalidKind
	}
	if u.Kind == KindScreenshot && !u.NoCustomerData {
		return ErrCustomerData
	}
	return nil
}

// apply writes the update onto an image.
func (u KindUpdate) apply(img *Image, now time.Time) {
	img.Kind, img.Device, img.NoCustomerDataBy, img.NoCustomerDataAt = u.Kind, "", "", ""
	if u.Kind == KindScreenshot {
		img.Device, img.NoCustomerDataBy, img.NoCustomerDataAt = u.Device, u.By, now.UTC().Format(time.RFC3339)
	}
}

// maxOf and minSideOf are the limits of a kind.
func (c Config) maxOf(kind string) int {
	if kind == KindScreenshot {
		return c.MaxScreens
	}
	return c.Max
}

func (c Config) minSideOf(kind string) int {
	if kind == KindScreenshot {
		return c.MinScreenSide
	}
	return c.MinShortSide
}

// countKind is the SQL count of a tenant's pictures of the kind bound as the next argument.
func countKind(b *sql.Builder, col, kind string) {
	b.WriteString("(SELECT count(*) FROM jsonb_array_elements(COALESCE(" + col + "->'" + MetadataKey +
		"', '[]'::jsonb)) AS k WHERE COALESCE(k->>'kind', '') = ")
	b.Arg(kind)
	b.WriteString(")")
}

// AddAs stores a validated upload as a picture of the update's kind.
func (s *Service) AddAs(ctx context.Context, tenantID uuid.UUID, up *objectstore.Upload, u KindUpdate) (View, error) {
	if err := u.check(); err != nil {
		return View{}, err
	}
	return s.addKind(ctx, tenantID, up, origin{}, u)
}

// SetKind marks one of the tenant's pictures as a screenshot, or back as a photo. The change is
// one conditional UPDATE, so a kind's maximum always holds.
func (s *Service) SetKind(ctx context.Context, tenantID uuid.UUID, id string, u KindUpdate) (View, error) {
	if err := u.check(); err != nil {
		return View{}, err
	}
	t, err := s.db.Tenant.Get(ctx, tenantID)
	if err != nil {
		return View{}, err
	}
	for _, img := range Read(t.Metadata) {
		if img.ID != id || !Owned(img.Key, tenantID) {
			continue
		}
		was := img.Kind
		if img.Width > 0 && min(img.Width, img.Height) < s.cfg.minSideOf(u.Kind) {
			return View{}, fmt.Errorf("%w: the picture is %dx%d px; the short side must be at least %d px",
				ErrTooSmall, img.Width, img.Height, s.cfg.minSideOf(u.Kind))
		}
		u.apply(&img, s.nowFn())
		n, err := s.replaceImage(ctx, tenantID, img, was)
		if err != nil {
			return View{}, err
		}
		if n == 0 {
			return View{}, ErrFull
		}
		if img.Kind != KindPhoto {
			s.endFacesAfter(ctx, tenantID, img.ID)
		}
		return s.view(img), nil
	}
	return View{}, ErrNotFound
}

// replaceImage puts img in place of the stored picture with its id, keeping the order, while its
// kind (when it changes) is below that kind's maximum.
func (s *Service) replaceImage(ctx context.Context, tenantID uuid.UUID, img Image, was string) (int, error) {
	patch, err := json.Marshal(img)
	if err != nil {
		return 0, err
	}
	col := enttenant.FieldMetadata
	return s.db.Tenant.Update().
		Where(enttenant.ID(tenantID), func(sel *sql.Selector) {
			if img.Kind == was {
				return
			}
			sel.Where(sql.P(func(b *sql.Builder) {
				countKind(b, sel.C(col), img.Kind)
				b.WriteString(" < ")
				b.Arg(s.cfg.maxOf(img.Kind))
			}))
		}).
		Modify(func(u *sql.UpdateBuilder) {
			u.Set(col, sql.ExprFunc(func(b *sql.Builder) {
				b.WriteString("jsonb_set(COALESCE(" + col + ", '{}'::jsonb), '{" + MetadataKey +
					"}', (SELECT COALESCE(jsonb_agg(CASE WHEN e->>'id' = ")
				b.Arg(img.ID)
				b.WriteString(" THEN ")
				b.Arg(string(patch))
				b.WriteString("::jsonb ELSE e END ORDER BY n), '[]'::jsonb) FROM jsonb_array_elements(COALESCE(" + col + "->'" +
					MetadataKey + "', '[]'::jsonb)) WITH ORDINALITY AS t(e, n)))")
			}))
		}).
		Save(ctx)
}

// Update handles PATCH /api/v1/tenant/brand-images/{id}.
// @Summary Mark a brand image as a screenshot of the business's app, or back as a photo
// @Tags BrandImages
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Image id"
// @Param request body KindUpdate true "kind: photo or screenshot; a screenshot needs no_customer_data true"
// @Success 200 {object} View
// @Failure 400 {object} httpapi.ErrorResponse
// @Failure 404 {object} httpapi.ErrorResponse
// @Failure 409 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /tenant/brand-images/{id} [patch]
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	var u KindUpdate
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&u); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}
	u.By = audit.Actor(r.Context())
	v, err := h.svc.SetKind(r.Context(), tid, chi.URLParam(r, "id"), u)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, v)
}

// uploadKind reads an upload's kind from the query: ?kind=screenshot&device=laptop&no_customer_data=true.
func uploadKind(r *http.Request) KindUpdate {
	q := r.URL.Query()
	return KindUpdate{Kind: q.Get("kind"), Device: q.Get("device"), NoCustomerData: q.Get("no_customer_data") == "true",
		By: audit.Actor(r.Context())}
}
