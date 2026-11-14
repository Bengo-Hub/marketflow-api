package custom_agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"entgo.io/ent/dialect/sql"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/platform/legaldocs"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// The veto-window mode (approval_mode auto_publish, Sprint 20, owner 2026-11-14) needs the plan
// feature autopilot_auto_publish and the tenant's acceptance of the current Autopilot terms. The
// feature is read strictly: an outage or the demo bypass never grants it. The acceptance and the
// tenant's hold categories live in Tenant.metadata (TermsKey, HoldCategoriesKey).

// Tenant metadata keys.
const (
	TermsKey          = "autopilot_terms"
	HoldCategoriesKey = "autopilot_hold_categories"
)

// DefaultTermsVersion is the Autopilot terms version a tenant accepts; a new version
// (MF_AUTOPILOT_TERMS_VERSION) asks every tenant again.
const DefaultTermsVersion = "2026-11-14"

// maxHoldCategories bounds the tenant's hold list.
const maxHoldCategories = 20

// TermsSummary is what the owner accepts, shown next to the switch. The draft under review is
// marketflow/docs/autopilot-auto-publish-terms-draft.md; keep the two the same.
var TermsSummary = []string{
	"When the veto window ends, the agent posts the finished video to the accounts you chose, unless you or an admin cancel it first.",
	"Before each post we email you what will go out, where and when, with a link that cancels it. Anyone holding that link can cancel.",
	"Paid boosts and ads never start by themselves. Each one needs your approval and a funded ad account.",
	"A video in a restricted category, in one of your hold categories, flagged by the compliance review or over your credit ceiling waits for your approval.",
	"The videos are made with AI and go out under your business name. A post you did not cancel counts as approved by you.",
	"Switching the agent off, or back to approving each ad, cancels every post still waiting.",
	"When these terms change you accept them again. Until then every video waits for your approval.",
}

// AutoPublishGate decides whether a tenant may use the veto-window mode.
type AutoPublishGate struct {
	DB *ent.Client
	// HasFeature reads a plan feature strictly (subscriptions StrictHasFeature); nil allows nothing.
	HasFeature   func(ctx context.Context, tenantID, feature string) bool
	TermsVersion string
	// TermsEffectiveAt is when TermsVersion applies (owner 2026-11-14: 14 days after it is
	// announced). Before it, a tenant that accepted an earlier version still counts as accepted;
	// the zero time applies the version at once.
	TermsEffectiveAt time.Time
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// CheckFeature, when set, replaces HasFeature and says whether its answer is definite
	// (subscriptions StrictFeature). An answer that is not definite never cancels a waiting post
	// (owner 2026-11-14); Status reports it as PlanUnknown.
	CheckFeature func(ctx context.Context, tenantID, feature string) (has, known bool)
	// Terms, when set, is auth-api's versioned terms document (owner 2026-11-14): the version to
	// accept and its effective date come from it, and acceptances are recorded there. Until a
	// version is published there, TermsVersion and TermsEffectiveAt (config) still apply and the
	// acceptance stays in tenant metadata. Nil keeps the config and metadata only.
	Terms TermsSource
	// TermsPageURL is the accounts site's page for the terms; a version's page is TermsPageURL/<version>.
	TermsPageURL string
}

// TermsSource is auth-api's terms document and the tenants' acceptances (legaldocs.Client). Its
// reads return the last known answer while auth-api is down, and an error only when there is none.
type TermsSource interface {
	Document(ctx context.Context) (legaldocs.Document, error)
	Accepted(ctx context.Context, tenantID uuid.UUID) ([]legaldocs.Acceptance, error)
	Accept(ctx context.Context, tenantID uuid.UUID, in legaldocs.AcceptInput) (legaldocs.Acceptance, error)
}

// Errors from accepting the terms.
var (
	// ErrTermsChanged: the version shown is no longer the one to accept (a new one was published).
	ErrTermsChanged = errors.New("the Autopilot terms changed, reload to read the new version")
	// ErrTermsUnavailable: auth-api could not be reached to read or record the terms.
	ErrTermsUnavailable = errors.New("the Autopilot terms could not be reached, try again shortly")
)

// Allowed reports whether the tenant may auto-publish now, and why not.
func (g AutoPublishGate) Allowed(ctx context.Context, tenantID uuid.UUID) (bool, string) {
	st, err := g.Status(ctx, tenantID)
	if err != nil {
		return false, "the plan could not be checked"
	}
	switch {
	case !st.PlanAllows:
		return false, "the plan does not include publishing after a veto window"
	case st.TermsUnknown:
		return false, "the Autopilot terms could not be checked"
	case !st.TermsAccepted:
		return false, "the Autopilot terms are not accepted"
	}
	return true, ""
}

// AutoPublishStatus is what the settings page shows about the veto-window mode.
// TermsEffectiveAt is when TermsVersion applies, when a date is set (RFC 3339). NewTermsPending is
// true while the tenant runs on an earlier accepted version before that date: it should accept
// the new terms before then, or auto-publish pauses. PlanUnknown is true when the plan could not
// be read just now (PlanAllows is then false, and nothing waiting is cancelled for it).
// TermsUnknown is the same for the terms: auth-api could not be read and nothing was known
// before, so TermsAccepted is false, nothing auto-publishes and nothing waiting is cancelled.
// TermsDocument is true when the version comes from auth-api's published document, and TermsURL
// is then its page on the accounts site.
type AutoPublishStatus struct {
	PlanAllows       bool     `json:"plan_allows"`
	PlanUnknown      bool     `json:"plan_unknown,omitempty"`
	TermsAccepted    bool     `json:"terms_accepted"`
	TermsUnknown     bool     `json:"terms_unknown,omitempty"`
	TermsVersion     string   `json:"terms_version"`
	TermsDocument    bool     `json:"terms_document,omitempty"`
	TermsURL         string   `json:"terms_url,omitempty"`
	TermsEffectiveAt string   `json:"terms_effective_at,omitempty"`
	NewTermsPending  bool     `json:"new_terms_pending,omitempty"`
	AcceptedAt       string   `json:"accepted_at,omitempty"`
	CanAutoPublish   bool     `json:"can_auto_publish"`
	HoldCategories   []string `json:"hold_categories"`
	Terms            []string `json:"terms"`
	DefaultVetoHours int      `json:"default_veto_hours"`
	MinVetoHours     int      `json:"min_veto_hours"`
	MaxVetoHours     int      `json:"max_veto_hours"`
}

// Status reads the tenant's plan feature, terms and hold categories.
func (g AutoPublishGate) Status(ctx context.Context, tenantID uuid.UUID) (AutoPublishStatus, error) {
	st := AutoPublishStatus{TermsVersion: g.version(), Terms: TermsSummary, HoldCategories: []string{},
		DefaultVetoHours: DefaultVeto, MinVetoHours: MinVetoHours, MaxVetoHours: MaxVetoHours}
	if g.DB == nil {
		return st, errors.New("no database")
	}
	t, err := g.DB.Tenant.Get(ctx, tenantID)
	if err != nil {
		return st, err
	}
	if g.CheckFeature != nil {
		has, known := g.CheckFeature(ctx, tenantID.String(), AutoPublishFeature)
		st.PlanAllows, st.PlanUnknown = has && known, !known
	} else {
		st.PlanAllows = g.HasFeature != nil && g.HasFeature(ctx, tenantID.String(), AutoPublishFeature)
	}
	legacyVersion, legacyAt := metadataAcceptance(t)
	if g.Terms != nil {
		g.termsFromSource(ctx, tenantID, &st, legacyVersion, legacyAt)
	} else {
		g.termsFromConfig(&st, legacyVersion, legacyAt)
	}
	st.HoldCategories = HoldCategories(t)
	st.CanAutoPublish = st.PlanAllows && st.TermsAccepted
	return st, nil
}

// metadataAcceptance is the acceptance kept in tenant metadata: every acceptance before the terms
// moved to auth-api, and any made while no version is published there. Still read as a fallback.
func metadataAcceptance(t *ent.Tenant) (version, acceptedAt string) {
	if terms, ok := t.Metadata[TermsKey].(map[string]any); ok {
		version, _ = terms["version"].(string)
		acceptedAt, _ = terms["accepted_at"].(string)
	}
	return version, acceptedAt
}

// termsFromConfig is the version and date from config (MF_AUTOPILOT_TERMS_*), accepted in metadata.
func (g AutoPublishGate) termsFromConfig(st *AutoPublishStatus, legacyVersion, legacyAt string) {
	if !g.TermsEffectiveAt.IsZero() {
		st.TermsEffectiveAt = g.TermsEffectiveAt.UTC().Format(time.RFC3339)
	}
	st.TermsAccepted = legacyVersion != "" && legacyVersion == st.TermsVersion
	st.AcceptedAt = legacyAt
	// An earlier accepted version still counts until the new one's effective date.
	if !st.TermsAccepted && legacyVersion != "" && g.now().Before(g.TermsEffectiveAt) {
		st.TermsAccepted, st.NewTermsPending = true, true
	}
}

// termsFromSource reads auth-api's document. The version to accept is the upcoming one when one is
// announced, else the one in force, and its effective date drives the notice: before it, a tenant
// that accepted the version in force (or, before any version is in force, any earlier terms,
// including the metadata acceptance) keeps auto-publishing. With nothing published in auth-api the
// config version applies as before. An unreadable answer with nothing known is TermsUnknown.
func (g AutoPublishGate) termsFromSource(ctx context.Context, tenantID uuid.UUID, st *AutoPublishStatus, legacyVersion, legacyAt string) {
	doc, err := g.Terms.Document(ctx)
	if err != nil {
		st.TermsUnknown = true
		return
	}
	if !doc.Published {
		g.termsFromConfig(st, legacyVersion, legacyAt)
		return
	}
	target := doc.Current
	if doc.Upcoming != nil {
		target = doc.Upcoming
	}
	st.TermsDocument = true
	st.TermsVersion = target.Version
	st.TermsURL = g.termsURL(target.Version)
	if !target.EffectiveAt.IsZero() {
		st.TermsEffectiveAt = target.EffectiveAt.UTC().Format(time.RFC3339)
	}

	accepted := map[string]string{}
	list, accErr := g.Terms.Accepted(ctx, tenantID)
	for _, a := range list {
		if _, ok := accepted[a.Version]; !ok {
			accepted[a.Version] = a.AcceptedAt.UTC().Format(time.RFC3339)
		}
	}
	// The metadata acceptance was of the in-app summary (config version), never of a published
	// document (owner 2026-11-14): it never counts as accepting an auth-api version, even one with
	// the same name. It only carries a tenant through the notice before the first version applies.

	if at, ok := accepted[target.Version]; ok {
		st.TermsAccepted, st.AcceptedAt = true, at
		return
	}
	if g.now().Before(target.EffectiveAt) {
		earlier := len(accepted) > 0 || legacyVersion != ""
		if doc.Current != nil {
			_, earlier = accepted[doc.Current.Version]
		}
		if earlier {
			st.TermsAccepted, st.NewTermsPending = true, true
			return
		}
	}
	if accErr != nil {
		st.TermsUnknown = true
	}
}

func (g AutoPublishGate) termsURL(version string) string {
	base := strings.TrimRight(strings.TrimSpace(g.TermsPageURL), "/")
	if base == "" {
		return ""
	}
	return base + "/" + url.PathEscape(version)
}

func (g AutoPublishGate) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

func (g AutoPublishGate) version() string {
	if g.TermsVersion != "" {
		return g.TermsVersion
	}
	return DefaultTermsVersion
}

// HoldCategories are the categories the tenant keeps out of the veto-window mode: an offering
// whose category or type is on the list always waits for a person (lower case).
func HoldCategories(t *ent.Tenant) []string {
	out := []string{}
	if t == nil {
		return out
	}
	var raw []string
	switch v := t.Metadata[HoldCategoriesKey].(type) {
	case []string:
		raw = v
	case []any:
		for _, x := range v {
			if s, ok := x.(string); ok {
				raw = append(raw, s)
			}
		}
	}
	for _, s := range raw {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// HeldCategory returns the tenant's hold category the offering falls in, or "" (exact category or
// type, and words; restricted categories need HeldFor with the matcher).
func HeldCategory(hold []string, offering map[string]any) string {
	return HeldFor(hold, offering, nil)
}

// RestrictedHoldKeys are the restricted categories a tenant can keep for its own approval
// ("Always ask me first for" in the Studio). They are creative.RestrictedCategoryKeys(); a test in
// autopublish keeps the two the same.
var RestrictedHoldKeys = []string{"alcohol", "betting", "children", "education", "finance", "health", "politics", "tobacco"}

// HeldFor returns the tenant's hold entry the offering falls under, or "" (owner 2026-11-14):
//   - an entry equal to the offering's category, category name or type;
//   - a restricted category key (RestrictedHoldKeys) whose terms the offering's name, title,
//     category, type or description touch, when restricted (creative.RestrictedCategories) is set;
//   - any other entry, a word or phrase the tenant added, found as whole words in those fields.
func HeldFor(hold []string, offering map[string]any, restricted func(texts ...string) []string) string {
	if len(hold) == 0 {
		return ""
	}
	for _, k := range []string{"category", "category_name", "type"} {
		v, _ := offering[k].(string)
		if v = strings.ToLower(strings.TrimSpace(v)); v != "" && slices.Contains(hold, v) {
			return v
		}
	}
	var texts []string
	for _, k := range []string{"name", "title", "category", "category_name", "type", "description"} {
		if v, _ := offering[k].(string); strings.TrimSpace(v) != "" {
			texts = append(texts, v)
		}
	}
	if len(texts) == 0 {
		return ""
	}
	if restricted != nil {
		touched := restricted(texts...)
		for _, h := range hold {
			if slices.Contains(RestrictedHoldKeys, h) && slices.Contains(touched, h) {
				return h
			}
		}
	}
	joined := strings.ToLower(strings.Join(texts, "\n"))
	for _, h := range hold {
		if slices.Contains(RestrictedHoldKeys, h) && restricted != nil {
			continue // matched by its terms above, not as a word
		}
		if containsWords(joined, h) {
			return h
		}
	}
	return ""
}

// containsWords reports whether phrase (lower case) appears in text (lower case) as whole words.
func containsWords(text, phrase string) bool {
	if phrase == "" {
		return false
	}
	for i := 0; ; {
		j := strings.Index(text[i:], phrase)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(phrase)
		if !wordByte(text, start-1) && !wordByte(text, end) {
			return true
		}
		i = start + 1
	}
}

// wordByte reports whether text[i] is a letter or digit (false outside the text). Bytes of a
// multi-byte letter count as letters, so a phrase inside a longer word does not match.
func wordByte(text string, i int) bool {
	if i < 0 || i >= len(text) {
		return false
	}
	c := text[i]
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c >= 0x80
}

// AutoPublishInput is the body of PUT /agents/autopilot/auto-publish.
type AutoPublishInput struct {
	AcceptTerms bool `json:"accept_terms"`
	// TermsVersion is the version the person read (terms_version from GET); when set and a newer
	// version was published since, the acceptance is refused with 409 terms_changed.
	TermsVersion   string   `json:"terms_version,omitempty"`
	HoldCategories []string `json:"hold_categories"`
}

// AcceptMeta is the browser's address and user agent, kept with an acceptance in auth-api.
type AcceptMeta struct {
	IP        string
	UserAgent string
}

// WithAutoPublishGate wires the plan feature check and the terms version.
func WithAutoPublishGate(hasFeature func(ctx context.Context, tenantID, feature string) bool, termsVersion string) func(*Service) {
	return func(s *Service) {
		s.gate.DB, s.gate.HasFeature, s.gate.TermsVersion = s.client, hasFeature, termsVersion
	}
}

// WithFeatureCheck sets the gate's definite feature check (subscriptions StrictFeature).
func WithFeatureCheck(check func(ctx context.Context, tenantID, feature string) (has, known bool)) func(*Service) {
	return func(s *Service) { s.gate.CheckFeature = check }
}

// WithTermsEffectiveAt sets when the current terms version applies (config
// AutopilotConfig.TermsEffective); the zero time applies it at once.
func WithTermsEffectiveAt(at time.Time) func(*Service) {
	return func(s *Service) { s.gate.TermsEffectiveAt = at }
}

// WithTermsSource reads the terms from auth-api (legaldocs.Client) and links each version's page
// under pageURL (config MF_AUTOPILOT_TERMS_PAGE_URL). A nil source keeps config and metadata.
func WithTermsSource(src TermsSource, pageURL string) func(*Service) {
	return func(s *Service) {
		if src != nil {
			s.gate.Terms = src
		}
		s.gate.TermsPageURL = pageURL
	}
}

// autoPublishGate is the service's gate; without WithAutoPublishGate nothing is allowed.
func (s *Service) autoPublishGate() AutoPublishGate {
	g := s.gate
	g.DB = s.client
	return g
}

// SetAutoPublish records the terms acceptance (who and when) and the hold categories, with one
// jsonb merge so other tenant metadata stays.
func (s *Service) SetAutoPublish(ctx context.Context, tenantID uuid.UUID, by *uuid.UUID, in AutoPublishInput) (AutoPublishStatus, error) {
	return s.SetAutoPublishFrom(ctx, tenantID, by, in, AcceptMeta{})
}

// SetAutoPublishFrom is SetAutoPublish with the browser's details for the acceptance record. Once
// auth-api has a published version, the acceptance is recorded there (with the user, address and
// user agent) and not in metadata; before that it goes to metadata as it always did.
func (s *Service) SetAutoPublishFrom(ctx context.Context, tenantID uuid.UUID, by *uuid.UUID, in AutoPublishInput, meta AcceptMeta) (AutoPublishStatus, error) {
	gate := s.autoPublishGate()
	patch := map[string]any{}
	if in.AcceptTerms {
		inMetadata := true
		if gate.Terms != nil {
			doc, err := gate.Terms.Document(ctx)
			if err != nil {
				return AutoPublishStatus{}, ErrTermsUnavailable
			}
			if doc.Published {
				inMetadata = false
				target := doc.Current
				if doc.Upcoming != nil {
					target = doc.Upcoming
				}
				if in.TermsVersion != "" && in.TermsVersion != target.Version {
					return AutoPublishStatus{}, ErrTermsChanged
				}
				_, err := gate.Terms.Accept(ctx, tenantID, legaldocs.AcceptInput{Version: target.Version, UserID: by, IP: meta.IP, UserAgent: meta.UserAgent})
				switch {
				case errors.Is(err, legaldocs.ErrNotAcceptable):
					return AutoPublishStatus{}, ErrTermsChanged
				case err != nil:
					s.logger.Warn("autopilot terms acceptance not recorded in auth-api", zap.String("tenant", tenantID.String()), zap.Error(err))
					return AutoPublishStatus{}, ErrTermsUnavailable
				}
			}
		}
		if inMetadata {
			if in.TermsVersion != "" && in.TermsVersion != gate.version() {
				return AutoPublishStatus{}, ErrTermsChanged
			}
			terms := map[string]any{"version": gate.version(), "accepted_at": time.Now().UTC().Format(time.RFC3339)}
			if by != nil {
				terms["accepted_by"] = by.String()
			}
			patch[TermsKey] = terms
		}
	}
	if in.HoldCategories != nil {
		hold := make([]string, 0, len(in.HoldCategories))
		for _, c := range in.HoldCategories {
			c = strings.ToLower(strings.TrimSpace(c))
			if c == "" || slices.Contains(hold, c) {
				continue
			}
			if len([]rune(c)) > 60 {
				return AutoPublishStatus{}, fmt.Errorf("%w: a category name is up to 60 characters", ErrInvalid)
			}
			hold = append(hold, c)
		}
		if len(hold) > maxHoldCategories {
			return AutoPublishStatus{}, fmt.Errorf("%w: up to %d categories", ErrInvalid, maxHoldCategories)
		}
		patch[HoldCategoriesKey] = hold
	}
	if len(patch) > 0 {
		raw, err := json.Marshal(patch)
		if err != nil {
			return AutoPublishStatus{}, err
		}
		value := sql.ExprFunc(func(b *sql.Builder) {
			b.WriteString("(COALESCE(" + tenant.FieldMetadata + ", '{}'::jsonb) || ")
			b.Arg(string(raw))
			b.WriteString("::jsonb)")
		})
		n, err := s.client.Tenant.Update().Where(tenant.IDEQ(tenantID)).
			Modify(func(u *sql.UpdateBuilder) { u.Set(tenant.FieldMetadata, value) }).
			Save(ctx)
		if err != nil {
			return AutoPublishStatus{}, err
		}
		if n == 0 {
			return AutoPublishStatus{}, &ent.NotFoundError{}
		}
	}
	return s.autoPublishGate().Status(ctx, tenantID)
}

// GetAutoPublish handles GET /api/v1/agents/autopilot/auto-publish.
// @Summary Whether the tenant may publish autopilot videos after a veto window
// @Tags Custom Agents
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} AutoPublishStatus
// @Security bearerAuth
// @Router /agents/autopilot/auto-publish [get]
func (h *Handler) GetAutoPublish(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	st, err := h.service.autoPublishGate().Status(r.Context(), tenantID)
	h.autoPublishReply(w, st, err)
}

// PutAutoPublish handles PUT /api/v1/agents/autopilot/auto-publish {accept_terms, hold_categories}:
// accepts the current Autopilot terms and sets the categories that always wait for a person. The
// approving roles only (CanApprove).
// @Summary Accept the Autopilot terms and set the hold categories
// @Tags Custom Agents
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param request body AutoPublishInput true "Terms and hold categories"
// @Success 200 {object} AutoPublishStatus
// @Failure 403 {object} httpapi.ErrorResponse
// @Failure 409 {object} httpapi.ErrorResponse "terms_changed: a newer terms version was published"
// @Failure 503 {object} httpapi.ErrorResponse "terms_unavailable: auth-api could not record the acceptance"
// @Security bearerAuth
// @Router /agents/autopilot/auto-publish [put]
func (h *Handler) PutAutoPublish(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	if !h.approver(w, r, tenantID) {
		return
	}
	var in AutoPublishInput
	if err := httpapi.DecodeJSON(r, &in); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}
	st, err := h.service.SetAutoPublishFrom(r.Context(), tenantID, callerID(r), in,
		AcceptMeta{IP: remoteIP(r), UserAgent: httpapi.UserAgent(r)})
	h.autoPublishReply(w, st, err)
}

// remoteIP is the caller's address as the ingress set it (ratelimit.TrustedRealIP rewrites
// RemoteAddr); a client-sent X-Forwarded-For is never read for the acceptance record.
func remoteIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func (h *Handler) autoPublishReply(w http.ResponseWriter, st AutoPublishStatus, err error) {
	switch {
	case err == nil:
		httpapi.JSON(w, http.StatusOK, st)
	case errors.Is(err, ErrTermsChanged):
		httpapi.Error(w, http.StatusConflict, "terms_changed", err.Error(), nil)
	case errors.Is(err, ErrTermsUnavailable):
		httpapi.Error(w, http.StatusServiceUnavailable, "terms_unavailable", err.Error(), nil)
	case errors.Is(err, ErrInvalid):
		httpapi.Error(w, http.StatusBadRequest, "invalid_request", err.Error(), nil)
	case ent.IsNotFound(err):
		httpapi.Error(w, http.StatusNotFound, "not_found", "tenant not found", nil)
	default:
		h.logger.Error("autopilot auto-publish settings", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "internal_error", "something went wrong", nil)
	}
}

// approver answers 403 unless the caller holds an approving role (CanApprove); nil allows all.
func (h *Handler) approver(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID) bool {
	if h.CanApprove != nil && !h.CanApprove(r.Context(), tenantID) {
		httpapi.Error(w, http.StatusForbidden, "forbidden", "only the account owner or an admin can change this", nil)
		return false
	}
	return true
}

// callerID is the signed-in user, nil for a service.
func callerID(r *http.Request) *uuid.UUID {
	if c, ok := authclient.ClaimsFromContext(r.Context()); ok && c != nil && !c.IsService {
		if id, err := uuid.Parse(c.Subject); err == nil {
			return &id
		}
	}
	return nil
}
