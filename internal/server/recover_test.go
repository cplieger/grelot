//go:build !windows

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A handler that panics answers 500 in grelot's own error body, whose
// "error" key the client reads, not in httpserver's default envelope.
func TestPanicAnswersGrelotErrorBody(t *testing.T) {
	h := newHarness(t)
	rec := httptest.NewRecorder()
	h.srv.middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/inbox", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	if body["error"] != "internal server error" || body["code"] != "internal_error" {
		t.Fatalf("body = %v, want grelot's internal_error body", body)
	}
	if _, ok := body["message"]; ok {
		t.Fatalf("body = %v carries httpserver's message key", body)
	}
}
