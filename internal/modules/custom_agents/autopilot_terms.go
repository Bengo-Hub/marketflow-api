package custom_agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"entgo.io/ent/dialect/sql"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/bengobox/marketflow-api/internal/httpapi"
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
	"The agent posts a finished video to the accounts you chose when the veto window ends, unless you cancel it first.",
	"Before each post you get an email saying what will be posted, where and when, with a link to cancel it.",
	"A video that touches a restricted category, or that the compliance review flags, always waits for your approval.",
	"The agent never starts a paid boost or ad. Each one needs your approval and a funded ad account.",
	"Posts go out under your business name. You can switch back to approving each ad at any time.",
}

// AutoPublishGate decides whether a tenant may use the veto-window mode.
type AutoPublishGate struct {
	DB *ent.Client
	// HasFeature reads a plan feature strictly (subscriptions StrictHasFeature); nil allows nothing.
	HasFeature   func(ctx context.Context, tenantID, feature string) bool
	TermsVersion string
}

// Allowed reports whether the tenant may auto-publish now, and why not.
func (g AutoPublishGate) Allowed(ctx context.Context, tenantID uuid.UUID) (bool, string) {
	st, err := g.Status(ctx, tenantID)
	if err != nil {
		return false, "the plan could not be checked"
	}
	switch {
	case !st.PlanAllows:
		return false, "the plan does not include publishing after a veto window"
	case !st.TermsAccepted:
		return false, "the Autopilot terms are not accepted"
	}
	return true, ""
}

// AutoPublishStatus is what the settings page shows about the veto-window mode.
type AutoPublishStatus struct {
	PlanAllows       bool     `json:"plan_allows"`
	TermsAccepted    bool     `json:"terms_accepted"`
	TermsVersion     string   `json:"terms_version"`
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
	st.PlanAllows = g.HasFeature != nil && g.HasFeature(ctx, tenantID.String(), AutoPublishFeature)
	if terms, ok := t.Metadata[TermsKey].(map[string]any); ok {
		v, _ := terms["version"].(string)
		st.TermsAccepted = v != "" && v == st.TermsVersion
		st.AcceptedAt, _ = terms["accepted_at"].(string)
	}
	st.HoldCategories = HoldCategories(t)
	st.CanAutoPublish = st.PlanAllows && st.TermsAccepted
	return st, nil
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

// HeldCategory returns the tenant's hold category the offering falls in, or "".
func HeldCategory(hold []string, offering map[string]any) string {
	for _, k := range []string{"category", "category_name", "type"} {
		v, _ := offering[k].(string)
		if v = strings.ToLower(strings.TrimSpace(v)); v != "" && slices.Contains(hold, v) {
			return v
		}
	}
	return ""
}

// AutoPublishInput is the body of PUT /agents/autopilot/auto-publish.
type AutoPublishInput struct {
	AcceptTerms    bool     `json:"accept_terms"`
	HoldCategories []string `json:"hold_categories"`
}

// WithAutoPublishGate wires the plan feature check and the terms version.
func WithAutoPublishGate(hasFeature func(ctx context.Context, tenantID, feature string) bool, termsVersion string) func(*Service) {
	return func(s *Service) {
		s.gate = AutoPublishGate{DB: s.client, HasFeature: hasFeature, TermsVersion: termsVersion}
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
	patch := map[string]any{}
	if in.AcceptTerms {
		terms := map[string]any{"version": s.autoPublishGate().version(), "accepted_at": time.Now().UTC().Format(time.RFC3339)}
		if by != nil {
			terms["accepted_by"] = by.String()
		}
		patch[TermsKey] = terms
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
	st, err := h.service.SetAutoPublish(r.Context(), tenantID, callerID(r), in)
	h.autoPublishReply(w, st, err)
}

func (h *Handler) autoPublishReply(w http.ResponseWriter, st AutoPublishStatus, err error) {
	switch {
	case err == nil:
		httpapi.JSON(w, http.StatusOK, st)
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
