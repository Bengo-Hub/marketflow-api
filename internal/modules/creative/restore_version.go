package creative

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/creativeasset"
	"github.com/bengobox/marketflow-api/internal/ent/creativebrief"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/modules/audit"
)

// Restoring a discarded version (owner, 2026-11-21): a discard keeps the draft for the tenant's
// window in case it was a mistake, and an admin can bring it back inside that window while its
// files are still stored. The version returns to ready (a script waiting for review returns to
// script_ready), its files get the normal draft time again, and its ad leaves the archive.
// Discards, restores and permanent deletes go to the tenant's activity log.

// Audit events for a version's life after a discard.
const (
	AuditVersionDiscarded = "creative.version_discarded"
	AuditVersionRestored  = "creative.version_restored"
	AuditVersionDeleted   = "creative.version_deleted"
)

var (
	ErrNotDiscarded = fmt.Errorf("%w: only a discarded version can be restored", ErrConflict)
	ErrFilesGone    = fmt.Errorf("%w: its files are no longer stored, so it cannot be restored", ErrConflict)
)

// RestoreVersion brings a discarded, never-approved version back.
func (s *Service) RestoreVersion(ctx context.Context, tenantID, id uuid.UUID) (*ent.CreativeVersion, error) {
	v, err := s.version(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if v.ApprovedAt != nil {
		return nil, ErrVersionApproved
	}
	if str(v.Metadata[metaDiscardedAt]) == "" {
		return nil, ErrNotDiscarded
	}
	assets, err := s.db.CreativeAsset.Query().Where(creativeasset.VersionID(id), creativeasset.TenantID(tenantID)).All(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range assets {
		if a.PurgedAt != nil {
			return nil, ErrFilesGone
		}
	}
	status := StatusReady
	if len(assets) == 0 {
		if _, ok := v.Copy["script"]; !ok || v.Kind != KindVideoAd {
			return nil, ErrFilesGone
		}
		status = StatusScriptReady
	}
	meta := copyMap(v.Metadata)
	delete(meta, metaDiscardedAt)
	delete(meta, metaDiscardPurgeAt)
	until := s.nowFn().Add(DraftTTL)
	tx, err := s.db.Tx(ctx)
	if err != nil {
		return nil, err
	}
	if err := tx.CreativeVersion.UpdateOneID(id).SetStatus(status).SetMetadata(meta).Exec(ctx); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if _, err := tx.CreativeAsset.Update().Where(creativeasset.VersionID(id), creativeasset.PurgedAtIsNil()).
		SetExpiresAt(until).Save(ctx); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if err := tx.CreativeBrief.Update().
		Where(creativebrief.ID(v.BriefID), creativebrief.TenantID(tenantID), creativebrief.Status("archived")).
		SetStatus("active").Exec(ctx); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	audit.Tenant(ctx, s.db, s.logger, tenantID, AuditVersionRestored, "creative_version", &id,
		map[string]any{"brief_id": v.BriefID.String(), "status": status})
	return s.version(ctx, tenantID, id)
}

// RestoreVersion handles POST /api/v1/creative/versions/{id}/restore: an admin brings back a
// discarded version while its files are still stored. 409 when it was not discarded, was
// approved or its files are gone; 403 for someone who may not approve.
// @Summary Restore a discarded version
// @Tags Creative
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Version id"
// @Success 200 {object} map[string]interface{}
// @Failure 403 {object} httpapi.ErrorResponse
// @Failure 409 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /creative/versions/{id}/restore [post]
func (h *Handler) RestoreVersion(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	if h.canApprove != nil && !h.canApprove(r.Context(), tid) {
		httpapi.Error(w, http.StatusForbidden, "forbidden", "only the account owner or an admin can restore a version", nil)
		return
	}
	v, err := h.svc.RestoreVersion(r.Context(), tid, id)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, v)
}
