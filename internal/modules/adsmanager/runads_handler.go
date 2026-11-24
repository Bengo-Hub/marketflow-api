package adsmanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent/campaign"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/platform/runads"
)

// runConfirmedKey marks a request whose Run ads plan code was verified (or not needed).
type runConfirmedKey struct{}

func runConfirmed(ctx context.Context) bool {
	v, _ := ctx.Value(runConfirmedKey{}).(bool)
	return v
}

// RunInput is the body of POST /ads/plan, /ads/run and /ads/run/otp.
type RunInput struct {
	VersionID string `json:"version_id"`
	Choice
	// Fingerprint is the plan the owner saw (POST /ads/plan); run and otp refuse another plan.
	Fingerprint string `json:"fingerprint,omitempty"`
	OTPID       string `json:"otp_id,omitempty"`
	OTPCode     string `json:"otp_code,omitempty"`
}

func readRunInput(w http.ResponseWriter, r *http.Request) (RunInput, uuid.UUID, bool) {
	var in RunInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
		return in, uuid.Nil, false
	}
	vid, err := uuid.Parse(in.VersionID)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_request", "version_id is required", nil)
		return in, uuid.Nil, false
	}
	return in, vid, true
}

func (h *Handler) planFailed(w http.ResponseWriter, err error) {
	if status, code, msg := planError(err); status != 0 {
		httpapi.Error(w, status, code, msg, nil)
		return
	}
	h.fail(w, err)
}

// Plan handles POST /api/v1/ads/plan {version_id, days?, total_cap?, platforms?}: the Run ads plan
// for an approved video, from the tenant's defaults with the owner's changes. Read only: nothing
// is created or spent.
// @Summary Run ads plan for an approved video
// @Tags Ads
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param request body RunInput true "The video and any changes"
// @Success 200 {object} RunPlan
// @Security bearerAuth
// @Router /ads/plan [post]
func (h *Handler) Plan(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	in, vid, ok := readRunInput(w, r)
	if !ok {
		return
	}
	plan, _, err := h.BuildPlan(r.Context(), tid, vid, in.Choice)
	if err != nil {
		h.planFailed(w, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, plan)
}

// RunOTP handles POST /api/v1/ads/run/otp: sends the WhatsApp confirmation code for a plan whose
// total is at or above the tenant's boost confirmation threshold.
// @Summary Send the confirmation code for a Run ads plan
// @Tags Ads
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /ads/run/otp [post]
func (h *Handler) RunOTP(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	in, vid, ok := readRunInput(w, r)
	if !ok {
		return
	}
	h.runOTP(w, r, tid, vid, in)
}

func (h *Handler) runOTP(w http.ResponseWriter, r *http.Request, tid, vid uuid.UUID, in RunInput) {
	if h.CanLaunch != nil && !h.CanLaunch(r.Context(), tid) {
		httpapi.Error(w, http.StatusForbidden, "forbidden", "only the account owner or an admin can run ads", nil)
		return
	}
	if h.Codes == nil {
		httpapi.Error(w, http.StatusServiceUnavailable, "otp_unavailable", "confirmation codes are not available", nil)
		return
	}
	plan, _, err := h.BuildPlan(r.Context(), tid, vid, in.Choice)
	if err != nil {
		h.planFailed(w, err)
		return
	}
	if plan.Fingerprint != in.Fingerprint {
		httpapi.Error(w, http.StatusConflict, "plan_changed", "the plan changed since it was shown; check it again", map[string]any{"plan": plan})
		return
	}
	h.sendCode(w, r, tid, "run:"+plan.Fingerprint)
}

// Run handles POST /api/v1/ads/run: the owner confirms the plan they saw (its fingerprint). Meta
// is boosted through Boost, Google Ads gets its campaign, TikTok gets its guided steps; each
// platform's result is recorded on its campaign row and on the video (metadata.ad_runs), and a
// second confirm of the same plan returns the first result instead of spending twice.
// @Summary Confirm and run a Run ads plan
// @Tags Ads
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param request body RunInput true "The plan's choices, its fingerprint and any code"
// @Success 201 {object} map[string]interface{}
// @Failure 409 {object} httpapi.ErrorResponse
// @Failure 428 {object} map[string]interface{}
// @Security bearerAuth
// @Router /ads/run [post]
func (h *Handler) Run(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	in, vid, ok := readRunInput(w, r)
	if !ok {
		return
	}
	if out, status, ok := h.run(w, r, tid, vid, in); ok {
		httpapi.JSON(w, status, out)
	}
}

// RunOutcome is what confirming a plan did.
type RunOutcome struct {
	Results []PlatformResult `json:"results"`
	Plan    *RunPlan         `json:"plan"`
	// Replayed is true when this plan had already been confirmed; nothing ran again.
	Replayed bool   `json:"replayed,omitempty"`
	Text     string `json:"text"`
}

// run checks and runs a confirmed plan. It writes any refusal itself (ok false).
func (h *Handler) run(w http.ResponseWriter, r *http.Request, tid, vid uuid.UUID, in RunInput) (RunOutcome, int, bool) {
	ctx := r.Context()
	if h.CanLaunch != nil && !h.CanLaunch(ctx, tid) {
		httpapi.Error(w, http.StatusForbidden, "forbidden", "only the account owner or an admin can run ads", nil)
		return RunOutcome{}, 0, false
	}
	plan, pc, err := h.BuildPlan(ctx, tid, vid, in.Choice)
	if err != nil {
		h.planFailed(w, err)
		return RunOutcome{}, 0, false
	}
	if plan.Fingerprint != in.Fingerprint {
		if prev, ok := h.storedRun(ctx, tid, vid, in.Fingerprint); ok {
			prev.Replayed = true
			return prev, http.StatusOK, true
		}
		httpapi.Error(w, http.StatusConflict, "plan_changed", "the plan changed since it was shown; check it again", map[string]any{"plan": plan})
		return RunOutcome{}, 0, false
	}
	if prev, ok := h.storedRun(ctx, tid, vid, plan.Fingerprint); ok {
		prev.Replayed = true
		return prev, http.StatusOK, true
	}
	if !plan.Ready {
		httpapi.Error(w, http.StatusConflict, "not_ready", strings.Join(planProblems(plan), "; "), map[string]any{"plan": plan})
		return RunOutcome{}, 0, false
	}
	// What an earlier try of this plan already did is kept; only what is left counts and runs.
	prevResults, prevAt := h.partialRun(ctx, tid, vid, plan.Fingerprint)
	done := map[string]PlatformResult{}
	for _, x := range prevResults {
		if !x.retryable() {
			done[x.Platform] = x
		}
	}
	var left int64
	for _, pp := range plan.Platforms {
		if _, ok := done[pp.Platform]; pp.Enabled && !ok {
			left += pp.Total
		}
	}
	// The agent ceiling on everything about to run together; Meta's boost checks its own again.
	if left > 0 && !h.withinAgentCeiling(w, r, tid, vid, left) {
		return RunOutcome{}, 0, false
	}
	if plan.OTPRequired {
		if strings.TrimSpace(in.OTPID) == "" || strings.TrimSpace(in.OTPCode) == "" {
			httpapi.JSON(w, http.StatusPreconditionRequired, map[string]any{"error": "otp_required", "code": "otp_required",
				"threshold_kes": h.boostOTPThreshold(ctx, tid)})
			return RunOutcome{}, 0, false
		}
		user, ok := actorOf(ctx)
		if !ok {
			httpapi.Error(w, http.StatusForbidden, "acting_user_required", "acting_user_required", nil)
			return RunOutcome{}, 0, false
		}
		switch err := h.Codes.verify(ctx, tid, user, "run:"+plan.Fingerprint, in.OTPID, in.OTPCode); {
		case errors.Is(err, errOTPInvalid):
			httpapi.Error(w, http.StatusUnprocessableEntity, "otp_invalid", err.Error(), nil)
			return RunOutcome{}, 0, false
		case err != nil:
			httpapi.Error(w, http.StatusServiceUnavailable, "otp_unavailable", "the confirmation code could not be checked; try again shortly", nil)
			return RunOutcome{}, 0, false
		}
	}
	claimed, err := h.claimRun(ctx, tid, vid, plan.Fingerprint, prevAt)
	if err != nil {
		h.fail(w, err)
		return RunOutcome{}, 0, false
	}
	if !claimed {
		if prev, ok := h.storedRun(ctx, tid, vid, plan.Fingerprint); ok {
			prev.Replayed = true
			return prev, http.StatusOK, true
		}
		httpapi.Error(w, http.StatusConflict, "run_in_progress", "this plan is already being run; check the campaigns in a minute", nil)
		return RunOutcome{}, 0, false
	}
	// The confirmed run finishes and is recorded even when the browser goes away.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	ctx = context.WithValue(ctx, runConfirmedKey{}, true)
	out := RunOutcome{Plan: plan, Results: []PlatformResult{}}
	for _, pp := range plan.Platforms {
		if !pp.Enabled {
			continue
		}
		if x, ok := done[pp.Platform]; ok {
			out.Results = append(out.Results, x)
			continue
		}
		switch pp.Platform {
		case runads.Meta:
			out.Results = append(out.Results, h.runMeta(ctx, r, tid, pc))
		case runads.Google:
			out.Results = append(out.Results, h.launchGoogle(ctx, tid, pc, pp, plan.Fingerprint))
		case runads.TikTok:
			out.Results = append(out.Results, h.runTikTok(ctx, tid, pc, pp, plan.Fingerprint))
		}
	}
	out.Text = ResultText(out.Results)
	status := runDone
	for _, x := range out.Results {
		if x.retryable() {
			status = runPartial
		}
	}
	now := h.RunAds.now().UTC()
	h.saveRun(ctx, tid, vid, plan.Fingerprint, map[string]any{"status": status, "at": now.Format(time.RFC3339Nano),
		"ends_at": now.AddDate(0, 0, plan.Days).Format(time.RFC3339), "results": out.Results})
	return out, http.StatusCreated, true
}

func planProblems(p *RunPlan) []string {
	out := append([]string{}, p.Blockers...)
	for _, pp := range p.Platforms {
		if !pp.Enabled {
			continue
		}
		for _, b := range pp.Blockers {
			out = append(out, platformName(pp.Platform)+": "+b)
		}
		for _, s := range pp.Setup {
			out = append(out, platformName(pp.Platform)+": "+s.Label)
		}
	}
	return out
}

func platformName(p string) string {
	switch p {
	case runads.Meta:
		return "Meta"
	case runads.Google:
		return "Google Ads"
	case runads.TikTok:
		return "TikTok"
	}
	return p
}

// runMeta boosts on Meta through the portal's Boost handler, unchanged: the platform switch, the
// launching roles, the quote, the agent ceiling, the launch credits and the PAUSED launch.
func (h *Handler) runMeta(ctx context.Context, r *http.Request, tid uuid.UUID, pc *planCtx) PlatformResult {
	res := PlatformResult{Platform: runads.Meta}
	if pc.meta == nil {
		res.Status, res.Message = "failed", "the Meta boost could not be prepared"
		return res
	}
	body, err := json.Marshal(pc.meta.Input)
	if err != nil {
		res.Status, res.Message = "failed", "the Meta boost could not be prepared"
		return res
	}
	r2 := r.Clone(ctx)
	r2.Body = io.NopCloser(bytes.NewReader(body))
	r2.ContentLength = int64(len(body))
	r2.Header.Set("X-Tenant-ID", tid.String())
	r2.Header.Set("Content-Type", "application/json")
	rec := &recorder{header: http.Header{}}
	h.Boost(rec, r2)
	var reply map[string]any
	_ = json.Unmarshal(rec.body.Bytes(), &reply)
	if rec.code == http.StatusCreated {
		res.Status, res.CampaignID = "created", str(reply["id"])
		res.Message = "Meta boost created. It starts paused and goes live once Meta approves the ad, usually within a day."
		return res
	}
	res.Status = "failed"
	res.Message = firstNonEmpty(str(reply["error"]), "Meta did not take the boost")
	// Boost refuses (4xx, 503) before anything is created. Any other answer may have left a boost
	// behind, so it is never tried again by itself.
	if rec.code >= http.StatusInternalServerError && rec.code != http.StatusServiceUnavailable {
		res.Status = "unknown"
		res.Message = "The Meta boost may or may not have been created; check Campaigns before boosting again."
	}
	return res
}

// runTikTok records the guided TikTok campaign, or creates it through the API once a launcher is
// registered and run_ads.tiktok_mode is "api".
func (h *Handler) runTikTok(ctx context.Context, tid uuid.UUID, pc *planCtx, pp PlatformPlan, runKey string) PlatformResult {
	if pp.Mode == "api" && h.RunAds.TikTok != nil {
		res, err := h.RunAds.TikTok.Launch(ctx, tid, pp, pc.asset.ID.String())
		if err == nil {
			return res
		}
		h.Logger.Warn("tiktok launch, falling back to guided", zap.Error(err))
	}
	res := PlatformResult{Platform: runads.TikTok, Steps: pp.Steps}
	row, err := h.runRow(ctx, tid, pc, pp, runKey, "tiktok")
	if err != nil {
		res.Status, res.Message = "failed", "the TikTok plan could not be recorded"
		return res
	}
	h.saveRunRow(ctx, row.ID, "guided", map[string]any{"mode": "guided", "steps": pp.Steps})
	res.Status, res.CampaignID = "guided", row.ID.String()
	res.Message = "TikTok is ready to set up: follow the steps, then mark it done in MarketFlow."
	return res
}

// TikTokDone handles POST /api/v1/ads/run/{id}/done: the owner finished a guided TikTok campaign.
// @Summary Mark a guided TikTok campaign as set up
// @Tags Ads
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Campaign UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /ads/run/{id}/done [post]
func (h *Handler) TikTokDone(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	if h.CanLaunch != nil && !h.CanLaunch(r.Context(), tid) {
		httpapi.Error(w, http.StatusForbidden, "forbidden", "only the account owner or an admin can run ads", nil)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_request", "campaign id required", nil)
		return
	}
	ctx := r.Context()
	n, err := h.DB.Campaign.Update().
		Where(campaign.IDEQ(id), campaign.TenantIDEQ(tid), campaign.PlatformEQ("tiktok"), campaign.StatusEQ("guided")).
		SetStatus(CampaignActive).Save(ctx)
	if err != nil {
		h.fail(w, err)
		return
	}
	if n == 0 {
		httpapi.Error(w, http.StatusConflict, "conflict", "this is not a TikTok campaign waiting to be set up", nil)
		return
	}
	h.saveRunRow(ctx, id, CampaignActive, map[string]any{"done_at": time.Now().UTC().Format(time.RFC3339)})
	httpapi.JSON(w, http.StatusOK, map[string]any{"id": id.String(), "status": CampaignActive})
}

// --- the run record on the video (metadata.ad_runs.{fingerprint}) ---

func adRunsPatch(fp string, v map[string]any) entsql.Querier {
	patch, _ := json.Marshal(map[string]any{fp: v})
	return entsql.ExprFunc(func(b *entsql.Builder) {
		b.WriteString("jsonb_set(COALESCE(" + creativeversion.FieldMetadata + ", '{}'::jsonb), '{ad_runs}', COALESCE(" +
			creativeversion.FieldMetadata + "->'ad_runs', '{}'::jsonb) || ")
		b.Arg(string(patch))
		b.WriteString("::jsonb)")
	})
}

// Run record statuses. A partial run had a platform that failed safely; confirming the plan again
// runs only that platform.
const (
	runDone    = "done"
	runPartial = "partial"
)

// retryable says a platform failed in a way that spent and created nothing.
func (r PlatformResult) retryable() bool { return r.Status == "failed" }

// partialRun is an earlier try of the plan that can be continued: its results and its time stamp.
func (h *Handler) partialRun(ctx context.Context, tid, vid uuid.UUID, fp string) ([]PlatformResult, string) {
	v, err := h.DB.CreativeVersion.Query().Where(creativeversion.IDEQ(vid), creativeversion.TenantIDEQ(tid)).Only(ctx)
	if err != nil {
		return nil, ""
	}
	rec := nested(nested(v.Metadata, "ad_runs"), fp)
	if str(rec["status"]) != runPartial {
		return nil, ""
	}
	b, _ := json.Marshal(rec["results"])
	var out []PlatformResult
	_ = json.Unmarshal(b, &out)
	return out, str(rec["at"])
}

// claimRun marks the plan as running on the video: the first confirm, or the continuation of the
// partial run stamped prevAt. Only one caller wins.
func (h *Handler) claimRun(ctx context.Context, tid, vid uuid.UUID, fp, prevAt string) (bool, error) {
	value := adRunsPatch(fp, map[string]any{"status": "running", "at": h.RunAds.now().UTC().Format(time.RFC3339Nano)})
	n, err := h.DB.CreativeVersion.Update().
		Where(creativeversion.IDEQ(vid), creativeversion.TenantIDEQ(tid), func(s *entsql.Selector) {
			s.Where(entsql.P(func(b *entsql.Builder) {
				col := s.C(creativeversion.FieldMetadata)
				if prevAt == "" {
					b.WriteString("NOT jsonb_exists(COALESCE(" + col + "->'ad_runs', '{}'::jsonb), ").Arg(fp).WriteString(")")
					return
				}
				b.WriteString("COALESCE(" + col + "->'ad_runs', '{}'::jsonb) @> ")
				want, _ := json.Marshal(map[string]any{fp: map[string]any{"status": runPartial, "at": prevAt}})
				b.Arg(string(want)).WriteString("::jsonb")
			}))
		}).
		Modify(func(u *entsql.UpdateBuilder) { u.Set(creativeversion.FieldMetadata, value) }).Save(ctx)
	return n == 1, err
}

func (h *Handler) saveRun(ctx context.Context, tid, vid uuid.UUID, fp string, v map[string]any) {
	value := adRunsPatch(fp, v)
	if _, err := h.DB.CreativeVersion.Update().Where(creativeversion.IDEQ(vid), creativeversion.TenantIDEQ(tid)).
		Modify(func(u *entsql.UpdateBuilder) { u.Set(creativeversion.FieldMetadata, value) }).Save(ctx); err != nil {
		h.Logger.Warn("run ads result not saved on the video", zap.String("version", vid.String()), zap.Error(err))
	}
}

// storedRun is a finished run of the plan, if there is one.
func (h *Handler) storedRun(ctx context.Context, tid, vid uuid.UUID, fp string) (RunOutcome, bool) {
	if fp == "" {
		return RunOutcome{}, false
	}
	v, err := h.DB.CreativeVersion.Query().Where(creativeversion.IDEQ(vid), creativeversion.TenantIDEQ(tid)).Only(ctx)
	if err != nil {
		return RunOutcome{}, false
	}
	rec := nested(nested(v.Metadata, "ad_runs"), fp)
	if str(rec["status"]) != "done" {
		return RunOutcome{}, false
	}
	b, _ := json.Marshal(rec["results"])
	var out RunOutcome
	if json.Unmarshal(b, &out.Results) != nil {
		return RunOutcome{}, false
	}
	out.Text = ResultText(out.Results)
	return out, true
}

// recorder captures a delegated handler's answer.
type recorder struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header { return r.header }
func (r *recorder) Write(b []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return r.body.Write(b)
}
func (r *recorder) WriteHeader(code int) {
	if r.code == 0 {
		r.code = code
	}
}

// --- WhatsApp (marketflow-ai) ---

// osRunInput is the body of the internal Run ads routes.
type osRunInput struct {
	TenantID    string `json:"tenant_id"`
	UserID      string `json:"user_id"`
	VersionID   string `json:"version_id"`
	Fingerprint string `json:"fingerprint,omitempty"`
	OTPID       string `json:"otp_id,omitempty"`
	OTPCode     string `json:"otp_code,omitempty"`
}

// readOSRun reads the body and acts as the person who tapped; their own role decides.
func (h *Handler) readOSRun(w http.ResponseWriter, r *http.Request) (osRunInput, uuid.UUID, uuid.UUID, *http.Request, bool) {
	var in osRunInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&in); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
		return in, uuid.Nil, uuid.Nil, nil, false
	}
	tid, err1 := uuid.Parse(in.TenantID)
	uid, err2 := uuid.Parse(in.UserID)
	vid, err3 := uuid.Parse(in.VersionID)
	if err1 != nil || err2 != nil || err3 != nil || uid == uuid.Nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_request", "tenant_id, user_id and version_id are required", nil)
		return in, uuid.Nil, uuid.Nil, nil, false
	}
	ctx := httpapi.WithActingUser(r.Context(), uid)
	if h.CanLaunch != nil && !h.CanLaunch(ctx, tid) {
		httpapi.Error(w, http.StatusForbidden, "forbidden", "only the account owner or an admin can run ads", nil)
		return in, uuid.Nil, uuid.Nil, nil, false
	}
	return in, tid, vid, r.Clone(ctx), true
}

// OSTurnRunPlan handles S2S POST /api/v1/internal/ads/os-turn/run-ads-plan: the tenant's default
// plan for an approved video, with the WhatsApp message. 200 {plan, text}.
func (h *Handler) OSTurnRunPlan(w http.ResponseWriter, r *http.Request) {
	_, tid, vid, r2, ok := h.readOSRun(w, r)
	if !ok {
		return
	}
	plan, _, err := h.BuildPlan(r2.Context(), tid, vid, Choice{})
	if err != nil {
		h.planFailed(w, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"plan": plan, "text": PlanText(plan), "ready": plan.Ready, "fingerprint": plan.Fingerprint})
}

// OSTurnRun handles S2S POST /api/v1/internal/ads/os-turn/run-ads: confirms the default plan the
// message showed (its fingerprint). 201 {results, text}; 428 otp_required; the portal's refusals.
func (h *Handler) OSTurnRun(w http.ResponseWriter, r *http.Request) {
	in, tid, vid, r2, ok := h.readOSRun(w, r)
	if !ok {
		return
	}
	if out, status, ok := h.run(w, r2, tid, vid, RunInput{VersionID: vid.String(), Fingerprint: in.Fingerprint,
		OTPID: in.OTPID, OTPCode: in.OTPCode}); ok {
		httpapi.JSON(w, status, out)
	}
}

// OSTurnRunOTP handles S2S POST /api/v1/internal/ads/os-turn/run-ads-otp: sends the code for the
// default plan the message showed.
func (h *Handler) OSTurnRunOTP(w http.ResponseWriter, r *http.Request) {
	in, tid, vid, r2, ok := h.readOSRun(w, r)
	if !ok {
		return
	}
	h.runOTP(w, r2, tid, vid, RunInput{VersionID: vid.String(), Fingerprint: in.Fingerprint})
}

// PlanText is the plan as a WhatsApp message (no MarketFlow links; marketflow-ai adds the Studio).
func PlanText(p *RunPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Ad plan for %d days, total %s %s", p.Days, p.Currency, thousands(p.Total))
	if p.TotalCap > 0 {
		fmt.Fprintf(&b, " (your cap %s %s)", p.Currency, thousands(p.TotalCap))
	}
	b.WriteString(":")
	for _, pp := range p.Platforms {
		if !pp.Enabled {
			continue
		}
		fmt.Fprintf(&b, "\n\n%s: %s %s a day", platformName(pp.Platform), p.Currency, thousands(pp.Daily))
		switch pp.Platform {
		case runads.Meta:
			b.WriteString(", boosted automatically")
		case runads.Google:
			b.WriteString(", campaign created for you")
		case runads.TikTok:
			b.WriteString(", you set it up with the links I send")
		}
		for _, e := range pp.Estimates {
			fmt.Fprintf(&b, "\n  About %s %s a %s (%s)", thousands(e.Value), e.Metric, e.Per, sourceWord(e.Source))
		}
		if len(pp.Estimates) == 0 && len(pp.Tiers) > 0 && pp.Tiers[0].Source == runads.SourceRuleOfThumb {
			b.WriteString("\n  Budget from MarketFlow's rule of thumb; no estimate yet")
		}
		if pp.Funding.Detail != "" {
			b.WriteString("\n  " + pp.Funding.Detail)
		}
		if pp.Funding.Link != "" && (pp.Funding.CanSpend == nil || !*pp.Funding.CanSpend) {
			b.WriteString("\n  Add money: " + pp.Funding.Link)
		}
		for _, s := range pp.Setup {
			b.WriteString("\n  To do first: " + s.Label)
		}
		for _, x := range pp.Blockers {
			b.WriteString("\n  Cannot run yet: " + x)
		}
	}
	for _, pp := range p.Platforms {
		if !pp.Enabled && len(pp.Setup) > 0 {
			fmt.Fprintf(&b, "\n\nNot included yet, %s: %s", platformName(pp.Platform), pp.Setup[0].Label)
		}
	}
	for _, x := range p.Blockers {
		b.WriteString("\n\nCannot run yet: " + x)
	}
	b.WriteString("\n\nNothing is spent until you confirm. To change budgets, days or platforms, open Run ads in the Studio.")
	return b.String()
}

func sourceWord(s string) string {
	switch s {
	case runads.SourcePlatform:
		return "the platform's estimate"
	case runads.SourceYourResults:
		return "from your past results"
	}
	return "rule of thumb"
}

// ResultText is what confirming did, as a message.
func ResultText(results []PlatformResult) string {
	var b strings.Builder
	for i, r := range results {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "%s: %s", platformName(r.Platform), r.Message)
		for _, s := range r.Steps {
			if s.URL != "" {
				fmt.Fprintf(&b, "\n  %s: %s", s.Label, s.URL)
			} else {
				b.WriteString("\n  " + s.Label)
			}
		}
	}
	return b.String()
}

// thousands writes 12345 as 12,345.
func thousands(n int64) string {
	s := fmt.Sprint(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var out []byte
	for i := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}
