package creative

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent/creativeasset"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/modules/audit"
)

// Deleting one version permanently (owner, 2026-11-21): an admin removes a draft that was never
// approved, such as an old script, without deleting the whole ad, and it is gone at once: its
// stored files are deleted, then its asset rows and its row, in one transaction. When a file cannot
// be deleted nothing is removed and the call fails, so no file is ever left without its row. The
// brief's last version is never deleted this way (deleting the ad is for that).

// Errors for a version that cannot be deleted on its own.
var (
	ErrVersionApproved = fmt.Errorf("%w: an approved version stays until its own retention ends", ErrConflict)
	ErrLastVersion     = fmt.Errorf("%w: this is the ad's last version; delete the ad instead", ErrConflict)
)

// DeleteVersion deletes a never-approved version now: its files, its asset rows and its row.
func (s *Service) DeleteVersion(ctx context.Context, tenantID, id uuid.UUID) error {
	v, err := s.version(ctx, tenantID, id)
	if err != nil {
		return err
	}
	if v.ApprovedAt != nil {
		return ErrVersionApproved
	}
	others, err := s.db.CreativeVersion.Query().
		Where(creativeversion.BriefID(v.BriefID), creativeversion.TenantID(tenantID), creativeversion.IDNEQ(id)).Count(ctx)
	if err != nil {
		return err
	}
	if others == 0 {
		return ErrLastVersion
	}
	if s.files == nil {
		return ErrFilesUnavailable
	}
	assets, err := s.db.CreativeAsset.Query().Where(creativeasset.VersionID(id), creativeasset.TenantID(tenantID)).All(ctx)
	if err != nil {
		return err
	}
	for _, a := range assets {
		if a.PurgedAt != nil || a.StorageKey == "" {
			continue
		}
		if err := s.files.Delete(ctx, a.StorageKey); err != nil {
			return fmt.Errorf("%w: %v", ErrFilesUnavailable, err)
		}
	}
	tx, err := s.db.Tx(ctx)
	if err != nil {
		return err
	}
	if _, err := tx.CreativeAsset.Delete().Where(creativeasset.VersionID(id), creativeasset.TenantID(tenantID)).Exec(ctx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.CreativeVersion.Delete().Where(creativeversion.ID(id), creativeversion.TenantID(tenantID), creativeversion.ApprovedAtIsNil()).Exec(ctx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	audit.Tenant(ctx, s.db, s.logger, tenantID, AuditVersionDeleted, "creative_version", &id,
		map[string]any{"brief_id": v.BriefID.String(), "files": len(assets)})
	s.archiveIfDone(ctx, tenantID, v.BriefID)
	return nil
}

// FileDeleter deletes a stored file by key (httpware objectstore).
type FileDeleter interface {
	Delete(ctx context.Context, key string) error
}

// WithFiles lets the service delete stored files at once (DeleteVersion); nil turns that off.
func (s *Service) WithFiles(f FileDeleter) *Service {
	s.files = f
	return s
}

// ErrFilesUnavailable means a version's files could not be deleted, so nothing was.
var ErrFilesUnavailable = errors.New("the files could not be deleted right now; nothing was removed, try again")

// DeleteVersion handles DELETE /api/v1/creative/versions/{id}: an admin deletes a never-approved
// version permanently. Its files and rows go in the next hourly purge. 409 for an approved
// version or the ad's last one; 403 for someone who may not approve.
// @Summary Delete a draft version permanently
// @Tags Creative
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Version id"
// @Success 200 {object} map[string]string
// @Failure 403 {object} httpapi.ErrorResponse
// @Failure 409 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /creative/versions/{id} [delete]
func (h *Handler) DeleteVersion(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	if h.canApprove != nil && !h.canApprove(r.Context(), tid) {
		httpapi.Error(w, http.StatusForbidden, "forbidden", "only the account owner or an admin can delete a version permanently", nil)
		return
	}
	if err := h.svc.DeleteVersion(r.Context(), tid, id); err != nil {
		h.fail(w, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
