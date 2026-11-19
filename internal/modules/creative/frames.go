package creative

import (
	"context"
	"strings"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/providersetting"
	"github.com/bengobox/marketflow-api/internal/modules/brandimages"
	"github.com/bengobox/marketflow-api/internal/modules/tenant"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// What a video or poster is made from (2026-11-19 audit, findings 4, 6 and 8).

// photoInfo describes one photo sent to marketflow-ai, by index: the offering's own photo, or a
// brand-library picture with what it shows. marketflow-ai uses a library picture as a scene's
// first frame only when its caption or tags match the scene, and never a logo.
type photoInfo struct {
	Library bool     `json:"library,omitempty"`
	Logo    bool     `json:"logo,omitempty"`
	Caption string   `json:"caption,omitempty"`
	Tags    []string `json:"tags,omitempty"`
}

// adPhotos is a video ad's photos as marketflow-ai fetches them: the offering's links, then
// fresh signed links to the brand images the ad was made with, each described in info. A brand
// image that cannot be linked is left out together with its description, so the two stay aligned.
func (r *Renderer) adPhotos(ctx context.Context, v *ent.CreativeVersion) ([]string, []photoInfo) {
	photos := append([]string{}, strs(v.Metadata["photo_urls"])...)
	info := make([]photoInfo, len(photos))
	keys := strs(v.Metadata[metaBrandImageKeys])
	if len(keys) == 0 || r.brandLinks == nil {
		return photos, info
	}
	stored := map[string]brandimages.Image{}
	if t, err := r.db.Tenant.Get(ctx, v.TenantID); err == nil {
		stored = brandimages.ByKey(t.Metadata, v.TenantID)
	}
	missing := 0
	for _, k := range keys {
		links := r.brandLinks.Links(v.TenantID, []string{k})
		if len(links) != 1 {
			missing++
			continue
		}
		img := stored[k]
		photos = append(photos, links[0])
		info = append(info, photoInfo{Library: true, Logo: brandimages.LooksLikeLogo(img), Caption: img.Caption, Tags: img.Tags})
	}
	if missing > 0 {
		r.logger.Warn("brand images not all linked; scenes are drawn without them",
			zap.String("version_id", v.ID.String()), zap.Int("keys", len(keys)), zap.Int("missing", missing))
	}
	return photos, info
}

// brandLogo is the only logo a tenant's ads carry (owner, 2026-11-19): a Forever tenant always
// gets the Forever Living logo; any other tenant its own profile logo, else the brief's brand
// block (itself taken from the profile when the brief was made). Codevertex shows its clients'
// logos on its website, so no logo is ever taken from a picture library.
func brandLogo(ctx context.Context, db *ent.Client, tenantID uuid.UUID, brief *ent.CreativeBrief) string {
	if t, err := db.Tenant.Get(ctx, tenantID); err == nil {
		if strings.EqualFold(t.BusinessType, tenant.BusinessTypeFLP) {
			return tenant.DefaultFBOLogo
		}
		if l := strings.TrimSpace(str(t.Metadata["logo_url"])); strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "http://") {
			return l
		}
	}
	if brief != nil {
		brand, _ := brief.Metadata["brand"].(map[string]any)
		return str(brand["logo_url"])
	}
	return ""
}

// Video formats. 1920x1080 is the default (owner, 2026-11-19): a platform setting
// (ProviderSetting platform / creative / video, key default_format) with a tenant override
// (Tenant.metadata video_format), both checked against the formats on offer.
const (
	defaultVideoFormat = "1920x1080"
	metaVideoFormat    = "video_format"
	videoSettingsType  = "creative"
	videoSettingsName  = "video"
	settingFormat      = "default_format"
)

// videoDefaultFormat is the format a new video ad gets when the request names none.
func videoDefaultFormat(ctx context.Context, db *ent.Client, tenantID uuid.UUID) string {
	if t, err := db.Tenant.Get(ctx, tenantID); err == nil {
		if f := strings.TrimSpace(str(t.Metadata[metaVideoFormat])); adFormats[f] {
			return f
		}
	}
	if row, err := db.ProviderSetting.Query().Where(providersetting.TenantIDEQ("platform"),
		providersetting.ProviderTypeEQ(videoSettingsType), providersetting.ProviderNameEQ(videoSettingsName),
		providersetting.KeyEQ(settingFormat), providersetting.IsActiveEQ(true)).First(ctx); err == nil && adFormats[strings.TrimSpace(row.Value)] {
		return strings.TrimSpace(row.Value)
	}
	return defaultVideoFormat
}
