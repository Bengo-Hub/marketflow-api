package handlers

import (
	"net/http"
	"time"

	"github.com/bengobox/marketflow-api/internal/httpapi"
)

// Health responds with basic service status.
func Health(w http.ResponseWriter, r *http.Request) {
	httpapi.JSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"service": "marketflow-api",
		"time":    time.Now().UTC(),
	})
}
