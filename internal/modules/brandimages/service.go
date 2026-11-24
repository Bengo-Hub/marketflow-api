package brandimages

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // decoders for the size check
	_ "image/png"
	"net/http"
	"regexp"
	"strings"
	"time"

	"entgo.io/ent/dialect/sql"
	"github.com/Bengo-Hub/httpware/objectstore"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	enttenant "github.com/bengobox/marketflow-api/internal/ent/tenant"
)

// service is the first key segment of every MarketFlow object (objectstore.New("marketflow", ...)).
const service = "marketflow"

// Errors the handler turns into answers.
var (
	// ErrNotFound is an image the tenant does not have (another tenant's included).
	ErrNotFound = errors.New("brand image not found")
	// ErrFull is an upload over the tenant's maximum.
	ErrFull = errors.New("brand image limit reached")
	// ErrTooSmall is a picture below the minimum short side.
	ErrTooSmall = errors.New("brand image too small")
	// ErrUnavailable is a server without object storage.
	ErrUnavailable = errors.New("file storage is not configured")
	// errStore is the object store refusing a write.
	errStore = errors.New("file storage is unavailable")
)

// Image is one stored brand image as kept in Tenant.metadata.brand_images. The key never leaves
// the server; the portal sees a View.
type Image struct {
	ID          string `json:"id"`
	Key         string `json:"key"`
	ContentType string `json:"content_type"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Bytes       int    `json:"bytes"`
	Name        string `json:"name,omitempty"`
	UploadedAt  string `json:"uploaded_at"`
	// Source is the web address of a picture linked from the business's own website.
	Source string `json:"source,omitempty"`
	// Page and Section say where on the website it was found (the page's address, "services").
	Page    string `json:"page,omitempty"`
	Section string `json:"section,omitempty"`
	// Hash is the picture's difference hash (16 hex digits), so a website import skips pictures
	// the business already has (dedupe.go).
	Hash string `json:"hash,omitempty"`
	// Caption and Tags say what the picture shows. A video uses a library picture as a scene's
	// first frame only when they match the scene; a picture without them is never a frame.
	Caption string   `json:"caption,omitempty"`
	Tags    []string `json:"tags,omitempty"`
	// Logo marks a logo (the business's or, on a software house's site, a client's). A logo is
	// never a video frame or a poster reference: a tenant's ads carry only its own logo, on the
	// end card.
	Logo bool `json:"logo,omitempty"`
	// Kind is KindPhoto or KindScreenshot (kinds.go); Device is where a screenshot is shown, and
	// NoCustomerDataBy and NoCustomerDataAt who said it shows no customer's data, and when.
	Kind             string `json:"kind,omitempty"`
	Device           string `json:"device,omitempty"`
	NoCustomerDataBy string `json:"no_customer_data_by,omitempty"`
	NoCustomerDataAt string `json:"no_customer_data_at,omitempty"`
}

// logoHint matches names and addresses of logo pictures and client strips.
var logoHint = regexp.MustCompile(`(?i)(logo|brandmark|wordmark|client|partner|sponsor|trusted|marquee)`)

// LooksLikeLogo reports whether a stored picture is, or very likely is, a logo: marked so, or
// named or linked like one.
func LooksLikeLogo(img Image) bool {
	return img.Logo || logoHint.MatchString(img.Name) || logoHint.MatchString(img.Source) || logoHint.MatchString(img.Caption)
}

// ByKey returns the tenant's stored pictures by key.
func ByKey(meta map[string]any, tenantID uuid.UUID) map[string]Image {
	out := map[string]Image{}
	for _, img := range Read(meta) {
		if Owned(img.Key, tenantID) {
			out[img.Key] = img
		}
	}
	return out
}

// View is an image as the portal sees it, with a short-lived link.
type View struct {
	ID          string `json:"id"`
	URL         string `json:"url"`
	ContentType string `json:"content_type"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Bytes       int    `json:"bytes"`
	Name        string `json:"name,omitempty"`
	UploadedAt  string `json:"uploaded_at"`
	// Source is set when the picture came from the business's website.
	Source string `json:"source,omitempty"`
	// From says where on the website, for the portal: "Services page" or "Home page, hero".
	From string `json:"from,omitempty"`
	// Kind and Device mark a screenshot of the business's app.
	Kind   string `json:"kind,omitempty"`
	Device string `json:"device,omitempty"`
}

// origin is where a stored picture came from; empty for an upload.
type origin struct {
	Source, Page, Section string
}

// Prefix is the key prefix of a tenant's brand images. A key outside it is never linked.
func Prefix(tenantID uuid.UUID) string {
	return service + "/" + tenantID.String() + "/" + MediaType + "/"
}

// Owned reports whether key is one of the tenant's brand image keys.
func Owned(key string, tenantID uuid.UUID) bool {
	return strings.HasPrefix(key, Prefix(tenantID)) && !strings.Contains(key, "..")
}

// Read returns the images kept in a tenant's metadata, oldest first.
func Read(meta map[string]any) []Image {
	raw, ok := meta[MetadataKey]
	if !ok {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var out []Image
	_ = json.Unmarshal(b, &out)
	return out
}

// Keys returns the tenant's brand photo keys from its metadata, at most n (n <= 0: all).
// Screenshots are not photos. Keys outside the tenant's prefix are dropped, so a bad metadata value
// cannot point at another tenant.
func Keys(meta map[string]any, tenantID uuid.UUID, n int) []string {
	var out []string
	for _, img := range OfKind(meta, tenantID, KindPhoto) {
		if n > 0 && len(out) >= n {
			break
		}
		out = append(out, img.Key)
	}
	return out
}

// Service stores, lists and deletes brand images.
type Service struct {
	db     *ent.Client
	store  *objectstore.Store
	cfg    Config
	logger *zap.Logger
	nowFn  func() time.Time
	// Website imports (siteimport.go): the tenant's own sites and the public-only client.
	siteLookup SiteLookup
	web        *http.Client
	// faceEnded purges what was made from a face whose consent ended (faces.go).
	faceEnded func(ctx context.Context, tenantID uuid.UUID, faceID string) error
}

// NewService builds the service and registers the media type with the configured size limit. A nil
// or unconfigured store answers ErrUnavailable on upload.
func NewService(db *ent.Client, store *objectstore.Store, cfg Config, logger *zap.Logger) *Service {
	register(cfg)
	return &Service{db: db, store: store, cfg: cfg, logger: logger.Named("brandimages"), nowFn: time.Now}
}

// Config returns the limits in force.
func (s *Service) Config() Config { return s.cfg }

// List returns the tenant's images with links valid for ListLinkTTL.
func (s *Service) List(ctx context.Context, tenantID uuid.UUID) ([]View, error) {
	t, err := s.db.Tenant.Get(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	out := []View{}
	for _, img := range Read(t.Metadata) {
		if !Owned(img.Key, tenantID) {
			continue
		}
		out = append(out, s.view(img))
	}
	return out, nil
}

func (s *Service) view(img Image) View {
	link := ""
	if s.store.Enabled() {
		if u, err := s.store.DownloadURL(img.Key, "", s.cfg.ListLinkTTL); err == nil {
			link = u
		}
	}
	return View{ID: img.ID, URL: link, ContentType: img.ContentType, Width: img.Width, Height: img.Height,
		Bytes: img.Bytes, Name: img.Name, UploadedAt: img.UploadedAt, Source: img.Source,
		From: FromLabel(img.Page, img.Section), Kind: img.Kind, Device: img.Device}
}

// CheckSize refuses a picture whose short side is below the minimum.
func (s *Service) CheckSize(data []byte) (width, height int, err error) {
	return checkSize(data, s.cfg.MinShortSide)
}

func checkSize(data []byte, minSide int) (width, height int, err error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, fmt.Errorf("%w: the image could not be read", ErrTooSmall)
	}
	short := cfg.Width
	if cfg.Height < short {
		short = cfg.Height
	}
	if short < minSide {
		return cfg.Width, cfg.Height, fmt.Errorf("%w: the picture is %dx%d px; the short side must be at least %d px",
			ErrTooSmall, cfg.Width, cfg.Height, minSide)
	}
	return cfg.Width, cfg.Height, nil
}

// Add stores a validated upload (objectstore.ReadUpload or Prepare) as one of the tenant's
// images. The list grows in one conditional UPDATE, so two uploads at once can never pass the
// maximum; the object of an upload that loses that race is deleted again.
func (s *Service) Add(ctx context.Context, tenantID uuid.UUID, up *objectstore.Upload) (View, error) {
	return s.add(ctx, tenantID, up, origin{})
}

// add stores one photo; from says where on the website a linked picture was found.
func (s *Service) add(ctx context.Context, tenantID uuid.UUID, up *objectstore.Upload, from origin) (View, error) {
	return s.addKind(ctx, tenantID, up, from, KindUpdate{})
}

// addKind stores one picture of a kind (kinds.go).
func (s *Service) addKind(ctx context.Context, tenantID uuid.UUID, up *objectstore.Upload, from origin, kind KindUpdate) (View, error) {
	source := from.Source
	if !s.store.Enabled() {
		return View{}, ErrUnavailable
	}
	w, h, err := checkSize(up.Data, s.cfg.minSideOf(kind.Kind))
	if err != nil {
		return View{}, err
	}
	t, err := s.db.Tenant.Get(ctx, tenantID)
	if err != nil {
		return View{}, err
	}
	if len(OfKind(t.Metadata, tenantID, kind.Kind)) >= s.cfg.maxOf(kind.Kind) {
		return View{}, ErrFull
	}
	key, err := s.store.NewKey(objectstore.Owner{TenantID: tenantID.String()}, MediaType, up.Ext)
	if err != nil {
		return View{}, err
	}
	if err := s.store.Put(ctx, key, up.Data, up.ContentType); err != nil {
		return View{}, fmt.Errorf("%w: %v", errStore, err)
	}
	name := strings.TrimSpace(up.FileName)
	if len([]rune(name)) > 120 {
		name = string([]rune(name)[:120])
	}
	img := Image{ID: uuid.NewString(), Key: key, ContentType: up.ContentType, Width: w, Height: h,
		Bytes: len(up.Data), Name: name, UploadedAt: s.nowFn().UTC().Format(time.RFC3339), Source: source,
		Page: from.Page, Section: from.Section}
	if h, ok := dHash(up.Data); ok {
		img.Hash = formatHash(h)
	}
	// A picture that is largely see-through is a logo or a cut-out mark, not a scene.
	img.Logo = kind.Kind == KindPhoto && (mostlyTransparent(up.Data) || logoHint.MatchString(name) || logoHint.MatchString(source))
	kind.apply(&img, s.nowFn())
	n, err := s.appendImage(ctx, tenantID, img)
	if err == nil && n == 0 {
		err = ErrFull
	}
	if err != nil {
		if derr := s.store.Delete(ctx, key); derr != nil {
			s.logger.Warn("brand image object left after a failed save", zap.String("key", key), zap.Error(derr))
		}
		return View{}, err
	}
	return s.view(img), nil
}

// appendImage adds img to the list only while its kind is below the maximum.
func (s *Service) appendImage(ctx context.Context, tenantID uuid.UUID, img Image) (int, error) {
	patch, err := json.Marshal([]Image{img})
	if err != nil {
		return 0, err
	}
	col := enttenant.FieldMetadata
	list := "COALESCE(" + col + "->'" + MetadataKey + "', '[]'::jsonb)"
	return s.db.Tenant.Update().
		Where(enttenant.ID(tenantID), func(sel *sql.Selector) {
			sel.Where(sql.P(func(b *sql.Builder) {
				countKind(b, sel.C(col), img.Kind)
				b.WriteString(" < ")
				b.Arg(s.cfg.maxOf(img.Kind))
			}))
		}).
		Modify(func(u *sql.UpdateBuilder) {
			u.Set(col, sql.ExprFunc(func(b *sql.Builder) {
				b.WriteString("jsonb_set(COALESCE(" + col + ", '{}'::jsonb), '{" + MetadataKey + "}', " + list + " || ")
				b.Arg(string(patch))
				b.WriteString("::jsonb)")
			}))
		}).
		Save(ctx)
}

// Delete removes one of the tenant's images: the list entry first (one UPDATE that keeps every
// other entry, including one added meanwhile), then the object. An id the tenant does not have is
// ErrNotFound, whoever owns it.
func (s *Service) Delete(ctx context.Context, tenantID uuid.UUID, id string) error {
	t, err := s.db.Tenant.Get(ctx, tenantID)
	if err != nil {
		return err
	}
	var found *Image
	for _, img := range Read(t.Metadata) {
		if img.ID == id {
			img := img
			found = &img
			break
		}
	}
	if found == nil || !Owned(found.Key, tenantID) {
		return ErrNotFound
	}
	col := enttenant.FieldMetadata
	_, err = s.db.Tenant.Update().
		Where(enttenant.ID(tenantID)).
		Modify(func(u *sql.UpdateBuilder) {
			u.Set(col, sql.ExprFunc(func(b *sql.Builder) {
				b.WriteString("jsonb_set(COALESCE(" + col + ", '{}'::jsonb), '{" + MetadataKey + "}', " +
					"(SELECT COALESCE(jsonb_agg(e), '[]'::jsonb) FROM jsonb_array_elements(COALESCE(" + col +
					"->'" + MetadataKey + "', '[]'::jsonb)) AS e WHERE e->>'id' <> ")
				b.Arg(id)
				b.WriteString("))")
			}))
		}).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("remove brand image: %w", err)
	}
	s.endFacesAfter(ctx, tenantID, id)
	// Hard delete (owner, 2026-11-14): the stored file goes too. A failed delete is recorded and
	// retried by the daily sweep, so the file never stays in storage.
	if s.store.Enabled() {
		if err := s.store.Delete(ctx, found.Key); err != nil && !errors.Is(err, objectstore.ErrNotFound) {
			s.logger.Warn("brand image object not deleted; queued for retry", zap.String("key", found.Key), zap.Error(err))
			s.rememberOrphan(ctx, tenantID, found.Key)
		}
	}
	return nil
}

// Linker turns brand image keys into signed links for marketflow-ai. It is what the creative
// renderer holds; a nil Linker links nothing.
type Linker struct {
	store *objectstore.Store
	ttl   time.Duration
}

// NewLinker builds a Linker whose links live for ttl (Config.RenderLinkTTL).
func NewLinker(store *objectstore.Store, ttl time.Duration) *Linker {
	return &Linker{store: store, ttl: ttl}
}

// Links signs each key that belongs to the tenant, in order. A key of another tenant, or one
// the store cannot sign, is skipped.
func (l *Linker) Links(tenantID uuid.UUID, keys []string) []string {
	if l == nil || !l.store.Enabled() {
		return nil
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if !Owned(k, tenantID) {
			continue
		}
		u, err := l.store.DownloadURL(k, "", l.ttl)
		if err != nil || u == "" {
			continue
		}
		out = append(out, u)
	}
	return out
}

// mostlyTransparent reports whether at least a fifth of a picture's pixels are see-through, the
// mark of a logo or icon rather than a photo. A picture that cannot be read is not.
func mostlyTransparent(data []byte) bool {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return false
	}
	b := img.Bounds()
	step := max(1, max(b.Dx(), b.Dy())/64)
	var clear, n int
	for y := b.Min.Y; y < b.Max.Y; y += step {
		for x := b.Min.X; x < b.Max.X; x += step {
			if _, _, _, a := img.At(x, y).RGBA(); a < 0x8000 {
				clear++
			}
			n++
		}
	}
	return n > 0 && clear*5 >= n
}
