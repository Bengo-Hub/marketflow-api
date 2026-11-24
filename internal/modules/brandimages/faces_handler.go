package brandimages

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/modules/audit"
)

// Audit events of brand faces (Settings, Activity log).
const (
	AuditFacesSwitched = "brand_faces.switched"
	AuditFaceRecorded  = "brand_faces.consent_recorded"
	AuditFaceEnded     = "brand_faces.consent_ended"
)

// faceView is a consent record as the portal shows it.
type faceView struct {
	Face
	// Live says whether the face may be used now.
	Live bool `json:"live"`
}

type facesReply struct {
	// Available is the platform's switch; On the tenant's own opt-in.
	Available bool       `json:"available"`
	On        bool       `json:"on"`
	MaxFaces  int        `json:"max_faces"`
	MaxDays   int        `json:"max_days"`
	Statement string     `json:"statement"`
	Faces     []faceView `json:"faces"`
}

// ListFaces handles GET /api/v1/tenant/brand-faces.
// @Summary List the tenant's brand face consents
// @Tags BrandImages
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} facesReply
// @Security bearerAuth
// @Router /tenant/brand-faces [get]
func (h *Handler) ListFaces(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	h.replyFaces(w, r, tid)
}

func (h *Handler) replyFaces(w http.ResponseWriter, r *http.Request, tid uuid.UUID) {
	t, err := h.svc.db.Tenant.Get(r.Context(), tid)
	if err != nil {
		h.fail(w, err)
		return
	}
	rules := FaceRulesFor(r.Context(), h.svc.db, tid)
	out := facesReply{Available: rules.Enabled, On: FacesOn(t.Metadata), MaxFaces: rules.MaxFaces, MaxDays: rules.MaxDays,
		Statement: FaceStatement, Faces: []faceView{}}
	live := map[string]bool{}
	for _, f := range LiveFaces(t.Metadata, tid, rules, h.svc.nowFn()) {
		live[f.ID] = true
	}
	for _, f := range ReadFaces(t.Metadata) {
		out.Faces = append(out.Faces, faceView{Face: f, Live: live[f.ID]})
	}
	httpapi.JSON(w, http.StatusOK, out)
}

// SwitchFaces handles PUT /api/v1/tenant/brand-faces ({"on": true|false}); admins only
// (rbac write gate). Off ends every consent.
// @Summary Turn brand faces on or off for the tenant
// @Tags BrandImages
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} facesReply
// @Failure 409 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /tenant/brand-faces [put]
func (h *Handler) SwitchFaces(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	var body struct {
		On *bool `json:"on"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil || body.On == nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "on must be true or false", nil)
		return
	}
	if err := h.svc.SetFacesOn(r.Context(), tid, *body.On, audit.Actor(r.Context())); err != nil {
		h.failFace(w, err)
		return
	}
	audit.Tenant(r.Context(), h.svc.db, h.logger, tid, AuditFacesSwitched, "tenant", &tid, map[string]any{"on": *body.On})
	h.replyFaces(w, r, tid)
}

// AddFace handles POST /api/v1/tenant/brand-faces; admins only.
// @Summary Record a person's consent to be a brand face
// @Description The picture must be one of the tenant's own brand images. under_18 must be answered; true is refused and nothing is stored.
// @Tags BrandImages
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param request body FaceInput true "Consent"
// @Success 201 {object} Face
// @Failure 400 {object} httpapi.ErrorResponse
// @Failure 409 {object} httpapi.ErrorResponse
// @Failure 422 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /tenant/brand-faces [post]
func (h *Handler) AddFace(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	var in FaceInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&in); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}
	in.By = audit.Actor(r.Context())
	f, err := h.svc.AddFace(r.Context(), tid, in)
	if err != nil {
		h.failFace(w, err)
		return
	}
	audit.Tenant(r.Context(), h.svc.db, h.logger, tid, AuditFaceRecorded, "brand_face", faceUUID(f.ID),
		map[string]any{"image_id": f.ImageID, "use": f.Use, "until": f.Until})
	httpapi.JSON(w, http.StatusCreated, f)
}

// RemoveFace handles DELETE /api/v1/tenant/brand-faces/{id}; admins only. The face stops being used
// at once, and actor stills made from it that are not in an approved ad are deleted. 502 when a
// file could not be deleted yet (it is retried hourly); the face is out of use either way.
// @Summary End a brand face consent
// @Tags BrandImages
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Face id"
// @Success 200 {object} facesReply
// @Failure 404 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /tenant/brand-faces/{id} [delete]
func (h *Handler) RemoveFace(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	err := h.svc.EndFace(r.Context(), tid, id, FaceRemoved, audit.Actor(r.Context()))
	if errors.Is(err, ErrFaceNotFound) {
		h.failFace(w, err)
		return
	}
	audit.Tenant(r.Context(), h.svc.db, h.logger, tid, AuditFaceEnded, "brand_face", faceUUID(id), map[string]any{"reason": FaceRemoved})
	if err != nil {
		h.logger.Error("brand face ended; its purge will be retried", zap.String("face_id", id), zap.Error(err))
		httpapi.Error(w, http.StatusBadGateway, "purge_pending",
			"The face is no longer used. Some of its pictures could not be deleted yet; we retry every hour.", nil)
		return
	}
	h.replyFaces(w, r, tid)
}

func faceUUID(id string) *uuid.UUID {
	u, err := uuid.Parse(id)
	if err != nil {
		return nil
	}
	return &u
}

func (h *Handler) failFace(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrFacesOff):
		httpapi.Error(w, http.StatusConflict, "brand_faces_off", err.Error(), nil)
	case errors.Is(err, ErrUnderage):
		httpapi.Error(w, http.StatusUnprocessableEntity, "under_18", err.Error(), nil)
	case errors.Is(err, ErrFaceInvalid):
		httpapi.Error(w, http.StatusBadRequest, "invalid_face", err.Error(), nil)
	case errors.Is(err, ErrFaceExists):
		httpapi.Error(w, http.StatusConflict, "face_exists", err.Error(), nil)
	case errors.Is(err, ErrFaceNotFound):
		httpapi.Error(w, http.StatusNotFound, "not_found", err.Error(), nil)
	case errors.Is(err, ErrFull):
		httpapi.Error(w, http.StatusConflict, "limit_reached", "you already have the most brand faces allowed; remove one first", nil)
	default:
		h.fail(w, err)
	}
}
