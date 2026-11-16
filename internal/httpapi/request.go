package httpapi

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
)

// MaxJSONBody is the largest JSON body a handler reads (audit M3).
const MaxJSONBody = 1 << 20

// ErrBodyTooLarge is returned by DecodeJSON for a body over MaxJSONBody.
var ErrBodyTooLarge = errors.New("request body too large")

// DecodeJSON reads the request body into v, at most MaxJSONBody bytes.
func DecodeJSON(r *http.Request, v any) error {
	defer r.Body.Close() //nolint:errcheck
	decoder := json.NewDecoder(http.MaxBytesReader(nil, r.Body, MaxJSONBody))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(v)
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return ErrBodyTooLarge
	}
	return err
}

// LimitJSONBody answers 413 for a JSON request whose declared length is over MaxJSONBody, before
// a handler reads it. A body with no declared length is cut off by DecodeJSON instead. Other
// content types (file uploads) are left to their handlers.
func LimitJSONBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > MaxJSONBody && isJSON(r.Header.Get("Content-Type")) {
			Error(w, http.StatusRequestEntityTooLarge, "body_too_large", "the request body is larger than 1 MB", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isJSON(contentType string) bool {
	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	return ct == "" || ct == "application/json" || strings.HasSuffix(ct, "+json")
}

// ClientIP attempts to extract caller IP considering proxy headers.
func ClientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		parts := strings.Split(forwarded, ",")
		return strings.TrimSpace(parts[0])
	}
	if realIP := r.Header.Get("X-Real-IP"); realIP != "" {
		return realIP
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// UserAgent returns the request user agent string.
func UserAgent(r *http.Request) string {
	return r.Header.Get("User-Agent")
}
