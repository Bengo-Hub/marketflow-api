package publishing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/ent/publishjob"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/modules/audit"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// AuditDeletedOnPlatform is the AuditLog event of a post deleted from its platform.
const AuditDeletedOnPlatform = "publishing.deleted_on_platform"

// Views builds calendar entries. For each published post it works out whether it can be deleted
// from the platform with the account's current permissions; the accounts come from one query.
func (s *EntStore) Views(ctx context.Context, tenantID uuid.UUID, rows []*ent.PublishJob) []JobView {
	var ids []uuid.UUID
	for _, r := range rows {
		if r.Status == StatusPublished {
			ids = append(ids, r.ConnectedAccountID)
		}
	}
	scopes := map[uuid.UUID][]string{}
	if len(ids) > 0 {
		accs, err := s.DB.ConnectedAccount.Query().
			Where(connectedaccount.IDIn(ids...), connectedaccount.TenantIDEQ(tenantID), connectedaccount.StatusEQ("active")).
			All(ctx)
		if err == nil {
			for _, a := range accs {
				scopes[a.ID] = append([]string{}, a.Scopes...)
			}
		}
	}
	out := make([]JobView, 0, len(rows))
	for _, r := range rows {
		v := view(r)
		if r.Status == StatusPublished {
			sc, connected := scopes[r.ConnectedAccountID]
			applyDelete(&v, deleteInfoFor(r.Platform, r.ExternalID, connected, sc))
		}
		out = append(out, v)
	}
	return out
}

func applyDelete(v *JobView, info DeleteInfo) {
	v.CanDeleteOnPlatform = info.CanDelete
	if !info.CanDelete {
		v.DeleteReason, v.DeleteSteps, v.DeleteHelpURL = info.Reason, info.Steps, info.HelpURL
	}
}

// deleteInfoFor is DeleteSupport plus the job's own conditions: a stored platform id and an
// account that is still connected.
func deleteInfoFor(platform, externalID string, connected bool, scopes []string) DeleteInfo {
	info := DeleteSupport(platform, scopes)
	if sp, ok := deleteSpecs[platform]; !ok || !sp.Supported {
		return info // the platform itself has no delete
	}
	switch {
	case externalID == "":
		info.CanDelete = false
		info.Reason = "MarketFlow has no " + title(platform) + " id for this post. Delete it on " + title(platform) + "."
	case !connected:
		info.CanDelete = false
		info.Reason = "the " + title(platform) + " account was disconnected. Reconnect it, or delete the post on " + title(platform) + "."
	}
	return info
}

// deletedDedupeKey frees the asset:account dedupe key of a post deleted from its platform, so the
// same video can be posted to the same account again (owner 2026-11-14). The job id keeps it
// unique if that new post is deleted too.
func deletedDedupeKey(key string, id uuid.UUID) string {
	return key + ":deleted:" + id.String()
}

// MarkDeletedOnPlatform records the delete with one conditional UPDATE (still published), merging
// deleted_at and deleted_by into metadata and freeing the dedupe key in the same statement. It
// reports whether this call made the change.
func (s *EntStore) MarkDeletedOnPlatform(ctx context.Context, tenantID, id uuid.UUID, dedupeKey, by string, at time.Time) (bool, error) {
	b, _ := json.Marshal(map[string]any{"deleted_at": at.UTC().Format(time.RFC3339), "deleted_by": by})
	value := entsql.ExprFunc(func(bd *entsql.Builder) {
		bd.WriteString("(COALESCE(" + publishjob.FieldMetadata + ", '{}'::jsonb) || ")
		bd.Arg(string(b))
		bd.WriteString("::jsonb)")
	})
	n, err := s.DB.PublishJob.Update().
		Where(publishjob.IDEQ(id), publishjob.TenantIDEQ(tenantID), publishjob.StatusEQ(StatusPublished)).
		SetStatus(StatusDeletedOnPlatform).
		SetDedupeKey(deletedDedupeKey(dedupeKey, id)).
		Modify(func(ub *entsql.UpdateBuilder) { ub.Set(publishjob.FieldMetadata, value) }).
		Save(ctx)
	return n > 0, err
}

// DeleteFromPlatform handles POST /api/v1/publishing/jobs/{id}/delete-from-platform: a post
// MarketFlow published is deleted from the platform, where the platform's API allows it (owner
// decision 2026-11-14). It is idempotent: a post already deleted answers 200 with the job. A
// platform that cannot delete answers 409 delete_unsupported with its own manual steps; a
// platform error answers 502 with the platform's message, and the job stays published.
// @Summary Delete a published post from its platform
// @Tags Publishing
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Job UUID"
// @Success 200 {object} JobView
// @Failure 403 {object} httpapi.ErrorResponse
// @Failure 409 {object} httpapi.ErrorResponse
// @Failure 502 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /publishing/jobs/{id}/delete-from-platform [post]
func (h *Handler) DeleteFromPlatform(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	can := h.CanDelete
	if can == nil {
		can = h.CanPost
	}
	if can != nil && !can(r.Context(), tid) {
		httpapi.Error(w, http.StatusForbidden, "forbidden", "only the account owner or an admin can delete a post from the platform", nil)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid id", nil)
		return
	}
	ctx := r.Context()
	row, err := h.Store.DB.PublishJob.Query().Where(publishjob.IDEQ(id), publishjob.TenantIDEQ(tid)).Only(ctx)
	if err != nil {
		h.fail(w, err)
		return
	}
	switch row.Status {
	case StatusDeletedOnPlatform:
		h.writeJob(w, r, tid, id)
		return
	case StatusPublished:
	default:
		httpapi.Error(w, http.StatusConflict, "conflict", "only a published post can be deleted from the platform", nil)
		return
	}

	tokens := h.Tokens
	if tokens == nil {
		tokens = h.Store
	}
	acct, accErr := tokens.Account(ctx, tid.String(), row.ConnectedAccountID.String())
	info := deleteInfoFor(row.Platform, row.ExternalID, accErr == nil, acct.Scopes)
	deleter := h.Deleters[row.Platform]
	if info.CanDelete && deleter == nil {
		info.CanDelete = false
		info.Reason = "deleting " + title(row.Platform) + " posts from MarketFlow is not available. Delete it on " + title(row.Platform) + "."
	}
	if !info.CanDelete {
		unsupported(w, info.Unsupported(row.Platform))
		return
	}

	alreadyGone := false
	if err := deleter.Delete(ctx, acct, row.ExternalID); err != nil {
		var un *DeleteUnsupportedError
		switch {
		case errors.As(err, &un):
			unsupported(w, un)
			return
		case errors.Is(err, ErrAlreadyGone):
			alreadyGone = true
		default:
			// A second request may have deleted it a moment ago; that one recorded it.
			if cur, gerr := h.Store.DB.PublishJob.Get(ctx, id); gerr == nil && cur.Status == StatusDeletedOnPlatform {
				h.writeJob(w, r, tid, id)
				return
			}
			h.Logger.Warn("delete from platform failed", zap.String("job", id.String()), zap.String("platform", row.Platform), zap.Error(err))
			httpapi.Error(w, http.StatusBadGateway, "platform_error", title(row.Platform)+" could not delete the post: "+err.Error(),
				map[string]any{"platform": row.Platform})
			return
		}
	}

	by := ""
	if u := userOf(r); u != nil {
		by = u.String()
	}
	changed, err := h.Store.MarkDeletedOnPlatform(ctx, tid, id, row.DedupeKey, by, time.Now())
	if err != nil {
		h.fail(w, err)
		return
	}
	if changed {
		audit.Tenant(ctx, h.Store.DB, h.Logger, tid, AuditDeletedOnPlatform, "publish_job", &id, map[string]any{
			"platform":     row.Platform,
			"external_id":  row.ExternalID,
			"permalink":    row.Permalink,
			"account_id":   row.ConnectedAccountID.String(),
			"deleted_by":   by,
			"already_gone": alreadyGone,
		})
	}
	h.writeJob(w, r, tid, id)
}

func (h *Handler) writeJob(w http.ResponseWriter, r *http.Request, tid, id uuid.UUID) {
	row, err := h.Store.DB.PublishJob.Query().Where(publishjob.IDEQ(id), publishjob.TenantIDEQ(tid)).Only(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, h.Store.Views(r.Context(), tid, []*ent.PublishJob{row})[0])
}

func unsupported(w http.ResponseWriter, e *DeleteUnsupportedError) {
	httpapi.Error(w, http.StatusConflict, "delete_unsupported", e.Reason, map[string]any{
		"platform": e.Platform,
		"steps":    e.Steps,
		"help_url": e.HelpURL,
	})
}
