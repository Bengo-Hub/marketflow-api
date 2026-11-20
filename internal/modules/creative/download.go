package creative

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/creativeasset"
	"github.com/bengobox/marketflow-api/internal/httpapi"
)

// Downloads (owner, 2026-11-20): the Studio's Download button saves the file. The stored file
// lives on another origin, where a browser ignores the download attribute and opens a new tab,
// so the API serves the tenant's own file with Content-Disposition: attachment and the Studio
// saves the response as a file.

// maxDownloadBytes caps one served file; finished ads are far smaller.
const maxDownloadBytes = 1 << 30

var downloadClient = &http.Client{Timeout: 5 * time.Minute}

// DownloadFile handles GET /api/v1/creative/assets/{id}/file: the asset as an attachment. A
// downloaded approved video starts its removal countdown, as the downloaded call does.
//
// @Summary Download a Studio file
// @Tags creative
// @Produce octet-stream
// @Param id path string true "Asset id"
// @Success 200 {file} binary
// @Failure 404 {object} httpapi.ErrorResponse
// @Router /creative/assets/{id}/file [get]
func (h *Handler) DownloadFile(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	a, err := h.svc.db.CreativeAsset.Query().
		Where(creativeasset.ID(id), creativeasset.TenantID(tid), creativeasset.PurgedAtIsNil()).
		Only(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	u, err := url.Parse(a.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		httpapi.Error(w, http.StatusNotFound, "not_found", "this file is no longer stored", nil)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, a.URL, nil)
	if err != nil {
		h.fail(w, err)
		return
	}
	resp, err := downloadClient.Do(req)
	if err != nil {
		h.logger.Warn("download fetch failed", zap.String("asset", id.String()), zap.Error(err))
		httpapi.Error(w, http.StatusBadGateway, "download_failed", "the file could not be read right now", nil)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		httpapi.Error(w, http.StatusNotFound, "not_found", "this file is no longer stored", nil)
		return
	}
	ctype := resp.Header.Get("Content-Type")
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", downloadName(a, u.Path, ctype)))
	if resp.ContentLength > 0 {
		w.Header().Set("Content-Length", fmt.Sprint(resp.ContentLength))
	}
	w.Header().Set("Cache-Control", "private, no-store")
	if _, err := io.Copy(w, io.LimitReader(resp.Body, maxDownloadBytes)); err != nil {
		return
	}
	if h.AfterUse != nil {
		_ = h.AfterUse.MarkDownloaded(r.Context(), tid, id)
	}
}

// downloadName is the saved file's name: marketflow-<kind>-<short id>.<ext>.
func downloadName(a *ent.CreativeAsset, urlPath, ctype string) string {
	ext := strings.ToLower(path.Ext(urlPath))
	if ext == "" || len(ext) > 5 {
		switch {
		case strings.Contains(ctype, "mp4"):
			ext = ".mp4"
		case strings.Contains(ctype, "png"):
			ext = ".png"
		default:
			ext = ".jpg"
		}
	}
	return fmt.Sprintf("marketflow-%s-%s%s", a.Kind, a.ID.String()[:8], ext)
}
