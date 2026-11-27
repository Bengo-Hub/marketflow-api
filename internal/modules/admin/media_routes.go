package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/marketflow-api/internal/ent/providersetting"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/platform/modelcatalog"
	"go.uber.org/zap"
)

// Media route models (owner, 2026-11-28). The model behind each media tier is a platform setting:
// ProviderSetting platform / media / routes, one key per tier (media.route.poster_premium, ...).
// marketflow-ai reads them (cached about a minute, plus its five minute settings cache), checks each
// id against fal's model list and runs the route's own default for a value it cannot use. A key
// that is not set is the route's default, so Reset deletes the key.
//
// The row is changed only through PATCH /admin/providers/media-routes, which merges: the keys sent
// change, every other key stays. POST /admin/providers/configure replaces every key of a provider,
// so it refuses media/routes.
const (
	mediaRoutesType = "media"
	mediaRoutesName = "routes"
	// previousSuffix keeps the value a change replaced, for the console's one-click revert.
	previousSuffix = ".previous"
)

// RouteSlot is one configurable media tier.
type RouteSlot struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	// Card is the provider card that edits it (fal or elevenlabs).
	Card string `json:"card"`
	// Kind is how the value is checked: fal (a fal endpoint id), elevenlabs (an ElevenLabs model
	// id) or flag ("true" or "false").
	Kind string `json:"kind"`
	// Categories are the fal categories that fit the tier, for the picker and the check.
	Categories []string `json:"categories,omitempty"`
	// Default is marketflow-ai's default for the tier (the same values are in its route_models.go;
	// it, not this list, decides what runs when the key is not set).
	Default string `json:"default"`
	Help    string `json:"help,omitempty"`
	// Legacy is an older setting on the ElevenLabs card the value falls back to before Default.
	Legacy string `json:"legacy,omitempty"`
}

// MediaRouteSlots are every tier the console can change.
var MediaRouteSlots = []RouteSlot{
	{Key: "media.route.poster_premium", Label: "Poster image (premium)", Card: "fal", Kind: "fal", Categories: []string{"text-to-image", "image-to-image"},
		Default: "fal-ai/nano-banana-pro", Help: "Text to image; its /edit sibling is used with the offering's photos."},
	{Key: "media.route.poster_standard", Label: "Poster image (standard)", Card: "fal", Kind: "fal", Categories: []string{"text-to-image", "image-to-image"},
		Default: "bytedance/seedream/v5/lite/text-to-image", Help: "Text to image; its /edit sibling is used with the offering's photos."},
	{Key: "media.route.video_premium", Label: "Video clip (premium)", Card: "fal", Kind: "fal", Categories: []string{"image-to-video"},
		Default: "fal-ai/kling-video/v3/pro/image-to-video", Help: "Animates the poster scene, so only image to video models fit."},
	{Key: "media.route.video_standard", Label: "Video clip (standard)", Card: "fal", Kind: "fal", Categories: []string{"image-to-video"},
		Default: "fal-ai/minimax/hailuo-2.3-fast/standard/image-to-video", Help: "Animates the poster scene, so only image to video models fit."},
	{Key: "media.route.presenter", Label: "Presenter (standard)", Card: "fal", Kind: "fal", Categories: []string{"image-to-video", "audio-to-video"},
		Default: "creatify/boreal", Help: "Lip-syncs the actor still to our voice-over."},
	{Key: "media.route.presenter_fabric", Label: "Presenter (alternative)", Card: "fal", Kind: "fal", Categories: []string{"image-to-video", "audio-to-video"},
		Default: "veed/fabric-1.0", Help: "The second fal presenter route."},
	{Key: "media.route.voice_fal_premium", Label: "Voice fallback on fal (premium)", Card: "fal", Kind: "fal", Categories: []string{"text-to-speech"},
		Default: "elevenlabs/tts/eleven-v4", Help: "Used only when ElevenLabs direct cannot read the voice-over. Premade voices only."},
	{Key: "media.route.voice_fal_standard", Label: "Voice fallback on fal (standard)", Card: "fal", Kind: "fal", Categories: []string{"text-to-speech"},
		Default: "elevenlabs/tts/eleven-v4-turbo", Help: "Used only when ElevenLabs direct cannot read the voice-over. Premade voices only."},
	{Key: "media.route.voice_timings_via_scribe", Label: "Time fal voice-overs with Scribe v2", Card: "fal", Kind: "flag",
		Default: "false", Help: "When fal returns no word timings, fal-ai/elevenlabs/speech-to-text/scribe-v2 (USD 0.008 a minute) times the audio for captions instead of an estimate."},
	{Key: "media.route.voice_premium", Label: "Voice (premium)", Card: "elevenlabs", Kind: "elevenlabs",
		Default: "eleven_v4", Legacy: "voice_premium_model", Help: "ElevenLabs direct, always tried first: library and cloned voices and exact word timings."},
	{Key: "media.route.voice_standard", Label: "Voice (standard)", Card: "elevenlabs", Kind: "elevenlabs",
		Default: "eleven_v4_turbo", Legacy: "voice_model", Help: "ElevenLabs direct, always tried first: library and cloned voices and exact word timings."},
}

func routeSlot(key string) (RouteSlot, bool) {
	for _, s := range MediaRouteSlots {
		if s.Key == key {
			return s, true
		}
	}
	return RouteSlot{}, false
}

var (
	falEndpointRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*(/[a-z0-9][a-z0-9._-]*)*$`)
	elevenModelRe = regexp.MustCompile(`^eleven_[a-z0-9_]+$`)
)

// errRouteValue is a value the caller must fix.
var errRouteValue = errors.New("invalid media route value")

// checkRouteValue checks v for the slot. fal is the fal model list (nil when it could not be read:
// the format is checked and the rest is left to marketflow-ai).
func checkRouteValue(s RouteSlot, v string, fal map[string]string) error {
	switch s.Kind {
	case "flag":
		if v != "true" && v != "false" {
			return fmt.Errorf("%w: %s is true or false", errRouteValue, s.Key)
		}
	case "elevenlabs":
		if !elevenModelRe.MatchString(v) {
			return fmt.Errorf("%w: %s is an ElevenLabs model id such as eleven_v4", errRouteValue, s.Key)
		}
	default:
		if !falEndpointRe.MatchString(v) {
			return fmt.Errorf("%w: %s is a fal endpoint id such as fal-ai/nano-banana-pro", errRouteValue, s.Key)
		}
		if fal == nil {
			return nil
		}
		cat, ok := fal[v]
		if !ok {
			return fmt.Errorf("%w: fal's model list does not have %s", errRouteValue, v)
		}
		if cat != "" && len(s.Categories) > 0 && !containsStr(s.Categories, cat) {
			return fmt.Errorf("%w: fal lists %s as %s; %s takes %s", errRouteValue, v, cat, s.Label, strings.Join(s.Categories, " or "))
		}
	}
	return nil
}

func containsStr(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// effectiveRoute is what runs for a slot: the setting, the legacy ElevenLabs card setting, then
// the default.
func effectiveRoute(s RouteSlot, routes, legacy map[string]string) string {
	if v := strings.TrimSpace(routes[s.Key]); v != "" {
		return v
	}
	if s.Legacy != "" {
		if v := strings.TrimSpace(legacy[s.Legacy]); v != "" {
			return v
		}
	}
	return s.Default
}

// planRoutePatch turns a patch into the rows to write and delete. Only keys in the patch change.
// An empty value resets the tier (the key and its previous value are deleted); a new value keeps
// the one it replaces under key.previous for revert. Unknown keys and the server-kept .previous
// keys are refused.
func planRoutePatch(patch, routes, legacy, fal map[string]string) (set map[string]string, del []string, err error) {
	set = map[string]string{}
	keys := make([]string, 0, len(patch))
	for k := range patch {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s, ok := routeSlot(k)
		if !ok {
			return nil, nil, fmt.Errorf("%w: unknown key %s", errRouteValue, k)
		}
		v := strings.TrimSpace(patch[k])
		if v == "" {
			del = append(del, k, k+previousSuffix)
			continue
		}
		if err := checkRouteValue(s, v, fal); err != nil {
			return nil, nil, err
		}
		if old := effectiveRoute(s, routes, legacy); old != v {
			set[k] = v
			if s.Kind != "flag" {
				set[k+previousSuffix] = old
			}
		}
	}
	return set, del, nil
}

// routeSettings reads the media/routes row.
func (h *PlatformProviderHandler) routeSettings(ctx context.Context, env string) (map[string]string, error) {
	rows, err := h.client.ProviderSetting.Query().
		Where(
			providersetting.TenantIDEQ(platformTenantID),
			providersetting.ProviderTypeEQ(mediaRoutesType),
			providersetting.ProviderNameEQ(mediaRoutesName),
			providersetting.EnvironmentEQ(env),
			providersetting.IsActiveEQ(true),
		).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, s := range rows {
		out[s.Key] = s.Value
	}
	return out, nil
}

// writeRouteSettings upserts set and deletes del in the media/routes row, leaving every other key
// as it is.
func (h *PlatformProviderHandler) writeRouteSettings(ctx context.Context, env string, set map[string]string, del []string) error {
	if len(del) > 0 {
		if _, err := h.client.ProviderSetting.Delete().
			Where(
				providersetting.TenantIDEQ(platformTenantID),
				providersetting.ProviderTypeEQ(mediaRoutesType),
				providersetting.ProviderNameEQ(mediaRoutesName),
				providersetting.EnvironmentEQ(env),
				providersetting.KeyIn(del...),
			).Exec(ctx); err != nil {
			return err
		}
	}
	for k, v := range set {
		n, err := h.client.ProviderSetting.Update().
			Where(
				providersetting.TenantIDEQ(platformTenantID),
				providersetting.ProviderTypeEQ(mediaRoutesType),
				providersetting.ProviderNameEQ(mediaRoutesName),
				providersetting.EnvironmentEQ(env),
				providersetting.KeyEQ(k),
			).
			SetValue(v).SetIsActive(true).SetStatus("active").
			Save(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		if err := h.client.ProviderSetting.Create().
			SetTenantID(platformTenantID).
			SetProviderType(mediaRoutesType).
			SetProviderName(mediaRoutesName).
			SetEnvironment(env).
			SetKey(k).
			SetValue(v).
			SetIsPlatform(true).
			SetIsPlatformManaged(true).
			SetIsActive(true).
			SetStatus("active").
			Exec(ctx); err != nil {
			return err
		}
	}
	return nil
}

// legacyVoiceSettings are the ElevenLabs card's voice_model and voice_premium_model.
func (h *PlatformProviderHandler) legacyVoiceSettings(ctx context.Context, env string) map[string]string {
	s, err := h.loadSettings(ctx, "media", "elevenlabs", env)
	if err != nil {
		return map[string]string{}
	}
	return s
}

// MediaRouteRow is one tier in the GET reply.
type MediaRouteRow struct {
	RouteSlot
	// Value is the saved setting ("" when the tier runs its default); Effective is what runs.
	Value     string `json:"value"`
	Effective string `json:"effective"`
	// Previous is the value the last change replaced, for revert ("" when none).
	Previous string `json:"previous,omitempty"`
}

// GetMediaRoutes handles GET /admin/providers/media-routes.
//
// @Summary Media route models
// @Description The model behind each media tier (poster, video, presenter, voice): the saved value, what runs, the default and the last value replaced.
// @Tags Admin - Providers
// @Produce json
// @Param card query string false "Only the tiers of one card: fal or elevenlabs"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /admin/providers/media-routes [get]
func (h *PlatformProviderHandler) GetMediaRoutes(w http.ResponseWriter, r *http.Request) {
	rows, err := h.mediaRouteRows(r.Context(), "production", r.URL.Query().Get("card"))
	if err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "query_failed", "failed to read the media routes", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"routes": rows})
}

func (h *PlatformProviderHandler) mediaRouteRows(ctx context.Context, env, card string) ([]MediaRouteRow, error) {
	routes, err := h.routeSettings(ctx, env)
	if err != nil {
		return nil, err
	}
	legacy := h.legacyVoiceSettings(ctx, env)
	var out []MediaRouteRow
	for _, s := range MediaRouteSlots {
		if card != "" && s.Card != card {
			continue
		}
		out = append(out, MediaRouteRow{RouteSlot: s, Value: routes[s.Key], Effective: effectiveRoute(s, routes, legacy),
			Previous: routes[s.Key+previousSuffix]})
	}
	return out, nil
}

type patchMediaRoutesRequest struct {
	Environment string            `json:"environment"`
	Settings    map[string]string `json:"settings"`
}

// PatchMediaRoutes handles PATCH /admin/providers/media-routes: the keys sent change and every
// other key stays. An empty value resets that tier to its default. A fal id is checked against
// fal's model list (cached from the last Sync models, or read now within a short budget).
//
// @Summary Change media route models
// @Description Merges the sent keys into the media routes (other keys are kept). An empty value resets a tier to its default; a fal id must be in fal's model list and fit the tier's category.
// @Tags Admin - Providers
// @Accept json
// @Produce json
// @Param request body patchMediaRoutesRequest true "Keys to change"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} map[string]string
// @Security bearerAuth
// @Router /admin/providers/media-routes [patch]
func (h *PlatformProviderHandler) PatchMediaRoutes(w http.ResponseWriter, r *http.Request) {
	var req patchMediaRoutesRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil || len(req.Settings) == 0 {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "settings with at least one key required", nil)
		return
	}
	if req.Environment == "" {
		req.Environment = "production"
	}
	ctx := r.Context()
	routes, err := h.routeSettings(ctx, req.Environment)
	if err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "query_failed", "failed to read the media routes", nil)
		return
	}
	fal, checked := h.falModels(ctx, req.Environment, needsFalCheck(req.Settings))
	set, del, err := planRoutePatch(req.Settings, routes, h.legacyVoiceSettings(ctx, req.Environment), fal)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_route", err.Error(), nil)
		return
	}
	if err := h.writeRouteSettings(ctx, req.Environment, set, del); err != nil {
		h.logger.Error("media routes not saved", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "save_failed", "failed to save the media routes", nil)
		return
	}
	h.auditRoutes(ctx, "admin.media_routes_updated", req.Settings)
	rows, _ := h.mediaRouteRows(ctx, req.Environment, "")
	httpapi.JSON(w, http.StatusOK, map[string]any{"routes": rows, "checked_against_fal": checked})
}

// needsFalCheck reports whether a patch sets a fal id.
func needsFalCheck(patch map[string]string) bool {
	for k, v := range patch {
		if s, ok := routeSlot(k); ok && s.Kind == "fal" && strings.TrimSpace(v) != "" {
			return true
		}
	}
	return false
}

func (h *PlatformProviderHandler) auditRoutes(ctx context.Context, event string, changes map[string]string) {
	actor := "system"
	if claims, ok := authclient.ClaimsFromContext(ctx); ok && claims != nil {
		actor = firstNonEmptyStr(claims.Email, claims.Subject, actor)
	}
	_, _ = h.client.AuditLog.Create().
		SetScope("platform").
		SetEvent(event).
		SetActor(actor).
		SetSubjectType("provider_setting").
		SetPayload(map[string]any{"provider_type": mediaRoutesType, "provider_name": mediaRoutesName, "changes": changes}).
		Save(ctx)
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// falModelCache keeps fal's model list (id to category) from the last Sync models or check.
type falModelCache struct {
	mu     sync.Mutex
	models map[string]string
	at     time.Time
}

const (
	falModelsTTL = 6 * time.Hour
	// falModelsBudget bounds a read made for a save, so a slow list never holds the form.
	falModelsBudget = 20 * time.Second
)

func (c *falModelCache) get() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.models == nil || time.Since(c.at) > falModelsTTL {
		return nil
	}
	return c.models
}

func (c *falModelCache) put(models []modelcatalog.Model) {
	m := make(map[string]string, len(models))
	for _, x := range models {
		m[x.ID] = x.Category
	}
	c.mu.Lock()
	c.models, c.at = m, time.Now()
	c.mu.Unlock()
}

// falModels returns fal's list, read now when none is cached and read is set. checked is false
// when it could not be read; the save then checks the format only and marketflow-ai checks again.
func (h *PlatformProviderHandler) falModels(ctx context.Context, env string, read bool) (map[string]string, bool) {
	if m := h.falCache.get(); m != nil {
		return m, true
	}
	if !read {
		return nil, false
	}
	settings, err := h.loadSettings(ctx, "media", "fal", env)
	key := strings.TrimSpace(settings["api_key"])
	if err != nil || key == "" {
		return nil, false
	}
	rctx, cancel := context.WithTimeout(ctx, falModelsBudget)
	defer cancel()
	listed, err := modelcatalog.ListModels(rctx, &http.Client{Timeout: 10 * time.Second}, "fal", key, settings["base_url"])
	if err != nil {
		h.logger.Warn("fal model list not read for the media route check; format checked only", zap.Error(err))
		return nil, false
	}
	h.falCache.put(listed)
	return h.falCache.get(), true
}

// RouteSwitch is one automatic route upgrade.
type RouteSwitch struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// autoPickRoutes moves each of the card's tiers to a newer model of its own family and tier, when
// the synced list has one (modelcatalog.NewerInFamily), keeps the replaced model for revert and
// tells the platform admins. A different family or a preview stays a recommendation.
func (h *PlatformProviderHandler) autoPickRoutes(ctx context.Context, card, env string, listed []modelcatalog.Model) []RouteSwitch {
	routes, err := h.routeSettings(ctx, env)
	if err != nil {
		h.logger.Warn("media routes not read for the automatic upgrade", zap.Error(err))
		return nil
	}
	legacy := h.legacyVoiceSettings(ctx, env)
	var fal map[string]string
	if card == "fal" {
		fal = map[string]string{}
		for _, m := range listed {
			fal[m.ID] = m.Category
		}
	}
	patch := map[string]string{}
	var switches []RouteSwitch
	for _, s := range MediaRouteSlots {
		if s.Card != card || s.Kind == "flag" {
			continue
		}
		cur := effectiveRoute(s, routes, legacy)
		next := modelcatalog.NewerInFamily(cur, listed)
		if next == "" || checkRouteValue(s, next, fal) != nil {
			continue
		}
		patch[s.Key] = next
		switches = append(switches, RouteSwitch{Key: s.Key, Label: s.Label, From: cur, To: next})
	}
	if len(switches) == 0 {
		return nil
	}
	set, del, err := planRoutePatch(patch, routes, legacy, fal)
	if err == nil {
		err = h.writeRouteSettings(ctx, env, set, del)
	}
	if err != nil {
		h.logger.Error("media route upgrade not saved", zap.Error(err))
		return nil
	}
	h.auditRoutes(ctx, "admin.media_routes_auto_upgraded", patch)
	h.tellRouteSwitches(ctx, card, switches)
	return switches
}

// Alerter is the platform alert path (worker.PlatformAlert: auth-api's Slack channel and email to
// the platform admins), the same one the model watch uses.
type Alerter interface {
	Send(ctx context.Context, subject, text, kind, key string)
}

// WithAlerts sets where automatic route upgrades are reported.
func (h *PlatformProviderHandler) WithAlerts(a Alerter) *PlatformProviderHandler {
	h.alerts = a
	return h
}

// tellRouteSwitches reports the upgrades in the background, bounded, so Sync models answers at once.
func (h *PlatformProviderHandler) tellRouteSwitches(ctx context.Context, card string, switches []RouteSwitch) {
	if h.alerts == nil {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Sync models on the %s card switched media routes to newer models of the same family:\n", card)
	for _, s := range switches {
		fmt.Fprintf(&b, "- %s: %s -> %s\n", s.Label, s.From, s.To)
	}
	b.WriteString("\nRecommendation: render one short test on each and compare quality and price with the previous model. " +
		"Platform > Providers shows a Revert button on each changed tier; no deploy is needed.")
	text := b.String()
	sum := sha256.Sum256([]byte(text))
	key := "media_route_upgrade:" + time.Now().UTC().Format("2006-01-02") + ":" + hex.EncodeToString(sum[:6])
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	go func() {
		defer cancel()
		h.alerts.Send(actx, "Media routes upgraded to newer models", text, "model_watch", key)
	}()
}

// falPriceCache keeps fal's prices per endpoint for a day.
type falPriceCache struct {
	mu     sync.Mutex
	prices map[string]FalPrice
	at     map[string]time.Time
}

// FalPrice is one endpoint's price.
type FalPrice struct {
	UnitPrice float64 `json:"unit_price"`
	Unit      string  `json:"unit"`
	Currency  string  `json:"currency"`
}

// falPricingBase is fal's pricing API (tests point it at a fake server).
var falPricingBase = "https://api.fal.ai/v1/models/pricing"

// FalPricing handles GET /admin/providers/fal-pricing?ids=a,b: each endpoint's price from fal's
// pricing API with the platform key, cached a day. Up to 50 ids.
//
// @Summary fal.ai prices
// @Description Unit price per endpoint from fal's pricing API (cached a day).
// @Tags Admin - Providers
// @Produce json
// @Param ids query string true "Comma-separated endpoint ids (up to 50)"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} map[string]string
// @Failure 502 {object} map[string]string
// @Security bearerAuth
// @Router /admin/providers/fal-pricing [get]
func (h *PlatformProviderHandler) FalPricing(w http.ResponseWriter, r *http.Request) {
	var ids []string
	for _, id := range strings.Split(r.URL.Query().Get("ids"), ",") {
		if id = strings.TrimSpace(id); id != "" && falEndpointRe.MatchString(id) && !containsStr(ids, id) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 || len(ids) > 50 {
		httpapi.Error(w, http.StatusBadRequest, "invalid_ids", "1 to 50 fal endpoint ids required", nil)
		return
	}
	out := map[string]FalPrice{}
	var missing []string
	h.falPrices.mu.Lock()
	for _, id := range ids {
		if p, ok := h.falPrices.prices[id]; ok && time.Since(h.falPrices.at[id]) < 24*time.Hour {
			out[id] = p
		} else {
			missing = append(missing, id)
		}
	}
	h.falPrices.mu.Unlock()
	if len(missing) > 0 {
		settings, err := h.loadSettings(r.Context(), "media", "fal", "production")
		key := strings.TrimSpace(settings["api_key"])
		if err != nil || key == "" {
			httpapi.Error(w, http.StatusBadRequest, "not_configured", "fal.ai has no api_key saved", nil)
			return
		}
		got, err := fetchFalPrices(r.Context(), key, missing)
		if err != nil {
			h.logger.Warn("fal pricing not read", zap.Error(err))
			httpapi.JSON(w, http.StatusOK, map[string]any{"prices": out, "error": "fal pricing could not be read"})
			return
		}
		h.falPrices.mu.Lock()
		if h.falPrices.prices == nil {
			h.falPrices.prices, h.falPrices.at = map[string]FalPrice{}, map[string]time.Time{}
		}
		for id, p := range got {
			out[id] = p
			h.falPrices.prices[id], h.falPrices.at[id] = p, time.Now()
		}
		h.falPrices.mu.Unlock()
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"prices": out})
}

// fetchFalPrices reads fal's pricing API ({prices: [{endpoint_id, unit_price, unit, currency}]},
// checked 2026-11-28) within ten seconds.
func fetchFalPrices(ctx context.Context, key string, ids []string) (map[string]FalPrice, error) {
	q := url.Values{}
	for _, id := range ids {
		q.Add("endpoint_id", id)
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, falPricingBase+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Key "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fal pricing: status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var v struct {
		Prices []struct {
			EndpointID string  `json:"endpoint_id"`
			UnitPrice  float64 `json:"unit_price"`
			Unit       string  `json:"unit"`
			Currency   string  `json:"currency"`
		} `json:"prices"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, fmt.Errorf("fal pricing: unreadable reply: %w", err)
	}
	out := map[string]FalPrice{}
	for _, p := range v.Prices {
		out[p.EndpointID] = FalPrice{UnitPrice: p.UnitPrice, Unit: p.Unit, Currency: p.Currency}
	}
	return out, nil
}
