package publishing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/Bengo-Hub/pagination"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/ent/creativeasset"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/bengobox/marketflow-api/internal/ent/publishjob"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Handler serves /api/v1/publishing: schedule an approved video to connected accounts, the
// calendar, cancel and retry. The worker does the posting.
type Handler struct {
	Store  *EntStore
	Logger *zap.Logger
	// CanPost gates scheduling, cancelling and retrying (a post goes out under the tenant's
	// name); nil allows every signed-in tenant user.
	CanPost func(ctx context.Context, tenantID uuid.UUID) bool
	// OnJobDone runs after a job is cancelled from the calendar (the worker covers the rest).
	OnJobDone func(ctx context.Context, jobID string)
}

// allowed answers 403 when the caller may not post for the tenant.
func (h *Handler) allowed(w http.ResponseWriter, r *http.Request, tid uuid.UUID) bool {
	if h.CanPost != nil && !h.CanPost(r.Context(), tid) {
		httpapi.Error(w, http.StatusForbidden, "forbidden", "only the account owner or an admin can post", nil)
		return false
	}
	return true
}

func NewHandler(store *EntStore, logger *zap.Logger) *Handler {
	return &Handler{Store: store, Logger: logger.Named("publishing.handler")}
}

// ScheduleInput is the body of POST /publishing/jobs.
type ScheduleInput struct {
	AssetID     string    `json:"asset_id"`
	AccountIDs  []string  `json:"connected_account_ids"`
	Caption     string    `json:"caption"`
	ScheduledAt time.Time `json:"scheduled_at"` // zero means now
	MadeForKids bool      `json:"made_for_kids"`
}

// JobView is one calendar entry.
type JobView struct {
	ID          string     `json:"id"`
	AssetID     string     `json:"asset_id"`
	AccountID   string     `json:"connected_account_id"`
	Platform    string     `json:"platform"`
	Caption     string     `json:"caption"`
	PlannedAt   time.Time  `json:"planned_at"`
	Status      string     `json:"status"`
	Permalink   string     `json:"permalink,omitempty"`
	Error       string     `json:"error,omitempty"`
	Attempts    int        `json:"attempts"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
}

const maxAccountsPerPost = 10

// Schedule handles POST /api/v1/publishing/jobs. Only an approved video may be posted; every
// account is checked (tenant, active, a platform that takes organic posts, its switch on) and
// preflighted before any row is written, so a bad request posts nothing. A repeat of the same
// asset and account returns the existing job (dedupe_key), never a second post.
// @Summary Schedule an approved video to connected accounts
// @Tags Publishing
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param request body ScheduleInput true "Post"
// @Success 201 {object} map[string]interface{}
// @Failure 400 {object} httpapi.ErrorResponse
// @Failure 409 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /publishing/jobs [post]
func (h *Handler) Schedule(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok || !h.allowed(w, r, tid) {
		return
	}
	var in ScheduleInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&in); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
		return
	}
	assetID, err := uuid.Parse(in.AssetID)
	if err != nil || len(in.AccountIDs) == 0 || len(in.AccountIDs) > maxAccountsPerPost {
		httpapi.Error(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("asset_id and 1 to %d connected_account_ids are required", maxAccountsPerPost), nil)
		return
	}
	now := time.Now().UTC()
	when := in.ScheduledAt.UTC()
	if when.IsZero() || when.Before(now) {
		when = now
	}
	if when.After(now.AddDate(0, 6, 0)) {
		httpapi.Error(w, http.StatusBadRequest, "invalid_request", "a post can be scheduled up to 6 months ahead", nil)
		return
	}
	ctx := r.Context()

	asset, err := h.Store.DB.CreativeAsset.Query().
		Where(creativeasset.IDEQ(assetID), creativeasset.TenantIDEQ(tid), creativeasset.KindEQ("video"), creativeasset.PurgedAtIsNil()).
		Only(ctx)
	if err != nil {
		h.fail(w, err)
		return
	}
	approved, err := h.Store.DB.CreativeVersion.Query().
		Where(creativeversion.IDEQ(asset.VersionID), creativeversion.TenantIDEQ(tid), creativeversion.StatusEQ("approved")).
		Exist(ctx)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !approved {
		httpapi.Error(w, http.StatusConflict, "not_approved", "approve the video before posting it", nil)
		return
	}
	fps, _ := asset.Metadata["fps"].(float64)
	video := Video{URL: asset.URL, Bytes: asset.Bytes, DurationMS: asset.DurationMs, Width: asset.Width, Height: asset.Height, FPS: fps}
	post := Post{Caption: strings.TrimSpace(in.Caption), AIGenerated: true, MadeForKids: in.MadeForKids}

	type target struct {
		acc      *ent.ConnectedAccount
		platform string
	}
	targets := make([]target, 0, len(in.AccountIDs))
	seen := map[uuid.UUID]bool{}
	for _, raw := range in.AccountIDs {
		aid, err := uuid.Parse(raw)
		if err != nil || seen[aid] {
			continue
		}
		seen[aid] = true
		acc, err := h.Store.DB.ConnectedAccount.Query().
			Where(connectedaccount.IDEQ(aid), connectedaccount.TenantIDEQ(tid), connectedaccount.StatusEQ("active")).
			Only(ctx)
		if err != nil {
			httpapi.Error(w, http.StatusBadRequest, "invalid_account", "one of the accounts is not connected or was disconnected", nil)
			return
		}
		platform := PlatformForAccountType(acc.AccountType)
		if platform == "" {
			httpapi.Error(w, http.StatusBadRequest, "invalid_account", fmt.Sprintf("%s cannot take an organic post", acc.DisplayName), nil)
			return
		}
		if !h.Store.Enabled(ctx, platform) {
			httpapi.Error(w, http.StatusConflict, "platform_disabled", fmt.Sprintf("posting to %s is not switched on yet", title(platform)), nil)
			return
		}
		if problems := Preflight(platform, video, post, acc.Scopes, 0); len(problems) > 0 {
			httpapi.Error(w, http.StatusBadRequest, "preflight_failed", title(platform)+": "+strings.Join(problems, "; "), map[string]any{"platform": platform, "problems": problems})
			return
		}
		targets = append(targets, target{acc, platform})
	}
	if len(targets) == 0 {
		httpapi.Error(w, http.StatusBadRequest, "invalid_request", "no valid connected_account_ids", nil)
		return
	}

	out := make([]JobView, 0, len(targets))
	for _, t := range targets {
		key := asset.ID.String() + ":" + t.acc.ID.String()
		c := h.Store.DB.PublishJob.Create().
			SetTenantID(tid).
			SetAssetID(asset.ID).
			SetConnectedAccountID(t.acc.ID).
			SetPlatform(t.platform).
			SetCaption(post.Caption).
			SetScheduledAt(when).
			SetDedupeKey(key).
			SetMetadata(map[string]any{
				"ai_generated":  true,
				"made_for_kids": post.MadeForKids,
				"planned_at":    when.Format(time.RFC3339),
			})
		if u := userOf(r); u != nil {
			c.SetCreatedBy(*u)
		}
		row, err := c.Save(ctx)
		if ent.IsConstraintError(err) {
			row, err = h.Store.DB.PublishJob.Query().Where(publishjob.DedupeKeyEQ(key), publishjob.TenantIDEQ(tid)).Only(ctx)
		}
		if err != nil {
			h.fail(w, err)
			return
		}
		out = append(out, view(row))
	}
	httpapi.JSON(w, http.StatusCreated, map[string]any{"data": out})
}

// AccountView is one account the publish dialog can post to.
type AccountView struct {
	ID          string `json:"id"`
	Platform    string `json:"platform"`
	DisplayName string `json:"display_name"`
	Enabled     bool   `json:"enabled"`         // the platform switch is on
	NeedsReauth bool   `json:"needs_reconnect"` // a required permission is missing
}

// Accounts handles GET /api/v1/publishing/accounts: the tenant's active accounts that take an
// organic post, with whether each platform is switched on. A tenant has a handful of accounts,
// so the list is bounded at 50 and not paginated.
// @Summary Accounts a video can be posted to
// @Tags Publishing
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /publishing/accounts [get]
func (h *Handler) Accounts(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	types := make([]string, 0, len(accountPlatforms))
	for t := range accountPlatforms {
		types = append(types, t)
	}
	rows, err := h.Store.DB.ConnectedAccount.Query().
		Where(connectedaccount.TenantIDEQ(tid), connectedaccount.StatusEQ("active"), connectedaccount.AccountTypeIn(types...)).
		Order(ent.Asc(connectedaccount.FieldAccountType), ent.Asc(connectedaccount.FieldDisplayName)).
		Limit(50).
		All(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	enabled := map[string]bool{}
	out := make([]AccountView, 0, len(rows))
	for _, a := range rows {
		p := PlatformForAccountType(a.AccountType)
		on, seen := enabled[p]
		if !seen {
			on = h.Store.Enabled(r.Context(), p)
			enabled[p] = on
		}
		out = append(out, AccountView{ID: a.ID.String(), Platform: p, DisplayName: a.DisplayName, Enabled: on,
			NeedsReauth: len(MissingScopes(p, a.Scopes)) > 0})
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"data": out})
}

// List handles GET /api/v1/publishing/jobs?from=&to=&status=&cursor=&limit=: the calendar,
// ordered by time with keyset pagination on (scheduled_at, id), served by the
// (tenant_id, scheduled_at, id) index. The window is at most 92 days.
// @Summary Publishing calendar
// @Tags Publishing
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param from query string false "RFC3339 start (default: 7 days ago)"
// @Param to query string false "RFC3339 end (default: 30 days ahead)"
// @Param status query string false "Filter by status"
// @Param cursor query string false "next_cursor of the previous page"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /publishing/jobs [get]
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	now := time.Now().UTC()
	from, to := now.AddDate(0, 0, -7), now.AddDate(0, 0, 30)
	if v := q.Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			httpapi.Error(w, http.StatusBadRequest, "invalid_request", "from must be RFC3339", nil)
			return
		}
		from = t
	}
	if v := q.Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			httpapi.Error(w, http.StatusBadRequest, "invalid_request", "to must be RFC3339", nil)
			return
		}
		to = t
	}
	if !to.After(from) || to.Sub(from) > 92*24*time.Hour {
		httpapi.Error(w, http.StatusBadRequest, "invalid_request", "the window must be positive and at most 92 days", nil)
		return
	}
	cp := pagination.ParseCursorParams(r)
	query := h.Store.DB.PublishJob.Query().
		Where(publishjob.TenantIDEQ(tid), publishjob.ScheduledAtGTE(from), publishjob.ScheduledAtLT(to))
	if s := q.Get("status"); s != "" {
		query = query.Where(publishjob.StatusEQ(s))
	}
	if cp.Cursor != "" {
		cid, cat, err := pagination.DecodeCursor(cp.Cursor)
		if err != nil {
			httpapi.Error(w, http.StatusBadRequest, "invalid_cursor", "invalid cursor", nil)
			return
		}
		query = query.Where(publishjob.Or(
			publishjob.ScheduledAtGT(cat),
			publishjob.And(publishjob.ScheduledAtEQ(cat), publishjob.IDGT(cid)),
		))
	}
	rows, err := query.Order(ent.Asc(publishjob.FieldScheduledAt), ent.Asc(publishjob.FieldID)).Limit(cp.Limit + 1).All(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	resp := pagination.CursorResponse[JobView]{Data: make([]JobView, 0, len(rows))}
	if len(rows) > cp.Limit {
		rows = rows[:cp.Limit]
		resp.HasMore = true
		last := rows[len(rows)-1]
		resp.NextCursor = pagination.EncodeCursor(last.ID, last.ScheduledAt)
	}
	for _, row := range rows {
		resp.Data = append(resp.Data, view(row))
	}
	httpapi.JSON(w, http.StatusOK, resp)
}

// Cancel handles POST /api/v1/publishing/jobs/{id}/cancel. A job still waiting or uploading
// stops; one the platform already has (processing, published) cannot be taken back here.
// @Summary Cancel a scheduled post
// @Tags Publishing
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Job UUID"
// @Success 200 {object} map[string]interface{}
// @Failure 409 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /publishing/jobs/{id}/cancel [post]
func (h *Handler) Cancel(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, []string{StatusPending, StatusUploading}, func(u *ent.PublishJobUpdate) *ent.PublishJobUpdate {
		return u.SetStatus(StatusCancelled)
	}, "only a post that has not reached the platform can be cancelled")
	// The video may now be fully used (its other posts done): the check is safe to run whether
	// or not this cancel applied, since it reads the jobs' current state.
	if h.OnJobDone != nil {
		h.OnJobDone(r.Context(), chi.URLParam(r, "id"))
	}
}

// Retry handles POST /api/v1/publishing/jobs/{id}/retry: a failed job starts again now, from
// the beginning.
// @Summary Retry a failed post
// @Tags Publishing
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Job UUID"
// @Success 200 {object} map[string]interface{}
// @Failure 409 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /publishing/jobs/{id}/retry [post]
func (h *Handler) Retry(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, []string{StatusFailed}, func(u *ent.PublishJobUpdate) *ent.PublishJobUpdate {
		return u.SetStatus(StatusPending).SetAttempts(0).SetError("").SetExternalID("").SetScheduledAt(time.Now().UTC()).
			Modify(func(ub *entsql.UpdateBuilder) { ub.Set(publishjob.FieldMetadata, clearUploadState) })
	}, "only a failed post can be retried")
}

// clearUploadState drops the stored upload position, so a retried job opens a new session.
var clearUploadState = entsql.Expr("(COALESCE(" + publishjob.FieldMetadata + ", '{}'::jsonb) - 'upload' - 'finished_at')")

func (h *Handler) transition(w http.ResponseWriter, r *http.Request, from []string, apply func(*ent.PublishJobUpdate) *ent.PublishJobUpdate, conflict string) {
	tid, ok := tenantOf(w, r)
	if !ok || !h.allowed(w, r, tid) {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid id", nil)
		return
	}
	// One conditional UPDATE, so a cancel and the worker's next save cannot both win.
	n, err := apply(h.Store.DB.PublishJob.Update().
		Where(publishjob.IDEQ(id), publishjob.TenantIDEQ(tid), publishjob.StatusIn(from...))).
		Save(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	if n == 0 {
		exists, _ := h.Store.DB.PublishJob.Query().Where(publishjob.IDEQ(id), publishjob.TenantIDEQ(tid)).Exist(r.Context())
		if !exists {
			httpapi.Error(w, http.StatusNotFound, "not_found", "not found", nil)
			return
		}
		httpapi.Error(w, http.StatusConflict, "conflict", conflict, nil)
		return
	}
	row, err := h.Store.DB.PublishJob.Get(r.Context(), id)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, view(row))
}

func view(row *ent.PublishJob) JobView {
	planned := row.ScheduledAt
	if v, ok := row.Metadata["planned_at"].(string); ok {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			planned = t
		}
	}
	return JobView{
		ID: row.ID.String(), AssetID: row.AssetID.String(), AccountID: row.ConnectedAccountID.String(),
		Platform: row.Platform, Caption: row.Caption, PlannedAt: planned, Status: row.Status,
		Permalink: row.Permalink, Error: row.Error, Attempts: row.Attempts, PublishedAt: row.PublishedAt,
	}
}

func tenantOf(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.Header.Get("X-Tenant-ID"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "X-Tenant-ID required", nil)
		return uuid.Nil, false
	}
	return id, true
}

func userOf(r *http.Request) *uuid.UUID {
	if c, ok := authclient.ClaimsFromContext(r.Context()); ok && c != nil {
		if id, err := uuid.Parse(c.Subject); err == nil {
			return &id
		}
	}
	return nil
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	switch {
	case ent.IsNotFound(err):
		httpapi.Error(w, http.StatusNotFound, "not_found", "not found", nil)
	case errors.Is(err, ErrDisabled):
		httpapi.Error(w, http.StatusConflict, "platform_disabled", err.Error(), nil)
	default:
		h.Logger.Error("publishing request failed", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "internal_error", "something went wrong", nil)
	}
}
