package learning

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Handler serves the internal learning routes (service key only; marketflow-ai is the caller).
type Handler struct {
	store  *Store
	logger *zap.Logger
}

func NewHandler(store *Store, logger *zap.Logger) *Handler {
	return &Handler{store: store, logger: logger.Named("learning")}
}

// Phrases handles GET /api/v1/internal/learning/phrases?tenant_id=&intent=.
func (h *Handler) Phrases(w http.ResponseWriter, r *http.Request) {
	tid, err := uuid.Parse(r.URL.Query().Get("tenant_id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "tenant_id must be a UUID", nil)
		return
	}
	list, err := h.store.Phrases(r.Context(), tid, clean(r.URL.Query().Get("intent")))
	if errors.Is(err, ErrInvalid) {
		httpapi.Error(w, http.StatusBadRequest, "invalid_intent", "intent is invalid", nil)
		return
	}
	if err != nil {
		h.logger.Warn("learned phrases", zap.Error(err))
	}
	if list == nil {
		list = []string{}
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"phrases": list})
}

// Record handles POST /api/v1/internal/learning/phrases {tenant_id, intent, phrase}.
func (h *Handler) Record(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TenantID string `json:"tenant_id"`
		Intent   string `json:"intent"`
		Phrase   string `json:"phrase"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<10)).Decode(&in); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
		return
	}
	tid, err := uuid.Parse(in.TenantID)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "tenant_id must be a UUID", nil)
		return
	}
	switch err := h.store.Record(r.Context(), tid, clean(in.Intent), in.Phrase); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, ErrInvalid):
		httpapi.Error(w, http.StatusBadRequest, "invalid_phrase", "the phrase or intent is not accepted", nil)
	default:
		h.logger.Warn("record learned phrase", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "internal", "could not record the phrase", nil)
	}
}
