package creative

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/funnel"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/platform/postcaption"
)

// PrivacyURLKey is the tenant metadata key of the business's own privacy policy link, which Meta
// shows on a leads form (owner, 2026-11-25). It is saved through the profile PATCH, which refuses
// MarketFlow's own pages; auth-api keeps no such link, so MarketFlow owns the setting.
const PrivacyURLKey = "privacy_url"

// TenantPrivacyURL is the business's own privacy policy link, "" when it has set none. There is no
// fallback: another business's policy is never used.
func TenantPrivacyURL(t *ent.Tenant) string {
	if t == nil {
		return ""
	}
	u := strings.TrimSpace(str(t.Metadata[PrivacyURLKey]))
	if !strings.HasPrefix(u, "https://") {
		return ""
	}
	return u
}

// PostKit prepares what a finished video is posted and advertised with (owner, 2026-11-25): its
// lead page (made when it has none), the links to it, the caption and the contacts. The Studio's
// Post, Run ads and Boost dialogs, WhatsApp and the autopilot all take it from here.
type PostKit struct {
	Svc  *Service
	Jobs *Renderer
	// Base is the portal's address, where lead pages are served (/f/{slug}).
	Base string
}

// PostPreset is what the dialogs and messages start with.
type PostPreset struct {
	VersionID string `json:"version_id"`
	// Caption is the whole post caption; Words are the ad's own words, the start of an ad's text.
	Caption  string   `json:"caption"`
	Words    string   `json:"words"`
	Contacts []string `json:"contacts"`
	// LeadURL and AdURL are the ad's lead page tagged for posts and for paid ads ("" without one).
	LeadURL string `json:"lead_url"`
	AdURL   string `json:"ad_url"`
	// PrivacyURL is the business's own privacy policy link; PrivacyPrompt says what to do without one.
	PrivacyURL    string `json:"privacy_url"`
	PrivacyPrompt string `json:"privacy_prompt,omitempty"`
}

// PrivacyPrompt is shown while a business has no privacy policy link.
const PrivacyPrompt = "Add your own privacy policy link in Settings, Public profile. Meta shows it on lead forms and needs your business's own policy."

// Preset prepares a version's post.
func (k *PostKit) Preset(ctx context.Context, tenantID, versionID uuid.UUID) (PostPreset, error) {
	v, err := k.Svc.version(ctx, tenantID, versionID)
	if err != nil {
		return PostPreset{}, err
	}
	return k.Compose(ctx, v, postcaption.Words(v.Copy)), nil
}

// Compose is Preset with the words given (the autopilot starts with the script's hook).
func (k *PostKit) Compose(ctx context.Context, v *ent.CreativeVersion, words string) PostPreset {
	p := PostPreset{VersionID: v.ID.String(), Words: words, Contacts: []string{}}
	b := k.LeadPage(ctx, v.TenantID, v.BriefID)
	title := ""
	if b != nil {
		title = b.Title
		if slug := k.publishedSlug(ctx, b); slug != "" {
			p.LeadURL = postcaption.LeadURL(k.Base, slug, b.ID.String(), "social", "social")
			p.AdURL = postcaption.LeadURL(k.Base, slug, b.ID.String(), "facebook", "paid")
		}
	}
	if k.Jobs != nil {
		p.Contacts = k.Jobs.ContactLines(ctx, v)
	}
	p.Caption = postcaption.Compose(postcaption.Parts{Words: words, Title: title, Contacts: p.Contacts, Link: p.LeadURL,
		PPB: postcaption.PPBLine(v.Metadata)})
	if t, err := k.Svc.db.Tenant.Get(ctx, v.TenantID); err == nil {
		p.PrivacyURL = TenantPrivacyURL(t)
	}
	if p.PrivacyURL == "" {
		p.PrivacyPrompt = PrivacyPrompt
	}
	return p
}

// LeadPage makes sure the ad has its lead page and that the tenant has a pipeline, once per brief.
// It never stops a post: a failure is logged and the brief is returned as it is (nil when it
// cannot be read).
func (k *PostKit) LeadPage(ctx context.Context, tenantID, briefID uuid.UUID) *ent.CreativeBrief {
	b, err := k.Svc.EnsureLeadFunnel(ctx, tenantID, briefID)
	if err == nil {
		return b
	}
	if !errors.Is(err, ErrNoLeadFunnels) {
		k.Svc.logger.Warn("lead page for a post not made", zap.String("brief_id", briefID.String()), zap.Error(err))
	}
	if b, err = k.Svc.brief(ctx, tenantID, briefID); err != nil {
		return nil
	}
	return b
}

// LeadPageOf is LeadPage for a version.
func (k *PostKit) LeadPageOf(ctx context.Context, tenantID, versionID uuid.UUID) {
	if v, err := k.Svc.version(ctx, tenantID, versionID); err == nil {
		k.LeadPage(ctx, tenantID, v.BriefID)
	}
}

// AdLink is the ad's lead page link for paid ads on a platform ("" without a published page).
func (k *PostKit) AdLink(ctx context.Context, v *ent.CreativeVersion, source string) string {
	b := k.LeadPage(ctx, v.TenantID, v.BriefID)
	if b == nil {
		return ""
	}
	return postcaption.LeadURL(k.Base, k.publishedSlug(ctx, b), b.ID.String(), source, "paid")
}

// publishedSlug is the ad's lead page slug while the page is published: an owner who archived it
// gets no link to a missing page.
func (k *PostKit) publishedSlug(ctx context.Context, b *ent.CreativeBrief) string {
	slug := str(b.Metadata["funnel_slug"])
	if slug == "" {
		return ""
	}
	ok, err := k.Svc.db.Funnel.Query().Where(funnel.TenantID(b.TenantID), funnel.SlugEQ(slug), funnel.StatusEQ("published")).Exist(ctx)
	if err != nil || !ok {
		return ""
	}
	return slug
}

// WithPostKit turns on the post preset route.
func (h *Handler) WithPostKit(k *PostKit) *Handler {
	h.postKit = k
	return h
}

// PostPresetFor handles POST /api/v1/creative/versions/{id}/post-preset: the caption, links and
// privacy policy link the Post, Run ads and Boost dialogs start with. It makes the ad's lead page
// when it has none, so it is a POST.
// @Summary What a finished video is posted and advertised with
// @Tags Creative
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Version ID"
// @Success 200 {object} PostPreset
// @Security bearerAuth
// @Router /creative/versions/{id}/post-preset [post]
func (h *Handler) PostPresetFor(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	if h.postKit == nil {
		httpapi.Error(w, http.StatusServiceUnavailable, "unavailable", "the post preset is not available right now", nil)
		return
	}
	p, err := h.postKit.Preset(r.Context(), tid, id)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, p)
}
