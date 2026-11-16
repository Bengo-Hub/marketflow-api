package httpapi

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Audit M3: JSON bodies are read up to 1 MB.
func TestDecodeJSONLimit(t *testing.T) {
	small := `{"name":"` + strings.Repeat("a", 100) + `"}`
	big := `{"name":"` + strings.Repeat("a", MaxJSONBody) + `"}`
	cases := []struct {
		name    string
		body    string
		tooBig  bool
		wantErr bool
	}{
		{"small body", small, false, false},
		{"over the limit", big, true, true},
		{"unknown field still refused", `{"other":1}`, false, true},
	}
	for _, c := range cases {
		var v struct {
			Name string `json:"name"`
		}
		// No declared length, as with a chunked body.
		r := httptest.NewRequest(http.MethodPost, "/", io.NopCloser(strings.NewReader(c.body)))
		r.ContentLength = -1
		err := DecodeJSON(r, &v)
		if (err != nil) != c.wantErr || errors.Is(err, ErrBodyTooLarge) != c.tooBig {
			t.Errorf("%s: err %v", c.name, err)
		}
	}
}

func TestLimitJSONBody(t *testing.T) {
	reached := false
	h := LimitJSONBody(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusNoContent)
	}))
	big := bytes.Repeat([]byte("a"), MaxJSONBody+1)
	cases := []struct {
		name        string
		contentType string
		body        []byte
		want        int
	}{
		{"small json", "application/json", []byte(`{}`), http.StatusNoContent},
		{"big json", "application/json; charset=utf-8", big, http.StatusRequestEntityTooLarge},
		{"big json without a type", "", big, http.StatusRequestEntityTooLarge},
		{"big upload is the handler's", "multipart/form-data; boundary=x", big, http.StatusNoContent},
	}
	for _, c := range cases {
		reached = false
		r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(c.body))
		if c.contentType != "" {
			r.Header.Set("Content-Type", c.contentType)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want || reached != (c.want == http.StatusNoContent) {
			t.Errorf("%s: status %d reached %v", c.name, w.Code, reached)
		}
	}
}
