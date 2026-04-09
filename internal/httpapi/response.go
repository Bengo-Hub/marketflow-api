package httpapi

import (
	"encoding/json"
	"net/http"
)

// JSON writes a JSON response with provided status code.
func JSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// ErrorResponse standard error envelope.
type ErrorResponse struct {
	Error   string         `json:"error"`
	Code    string         `json:"code,omitempty"`
	Details map[string]any `json:"details,omitempty"`
}

// Error writes an error response.
func Error(w http.ResponseWriter, status int, code string, message string, details map[string]any) {
	JSON(w, status, ErrorResponse{
		Error:   message,
		Code:    code,
		Details: details,
	})
}
