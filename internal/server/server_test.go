package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"agentrm/internal/api"
	"agentrm/internal/manager"
)

func post(h http.Handler, path, version string, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	if version != "" {
		r.Header.Set(api.VersionHeader, version)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestVersionAndErrors(t *testing.T) {
	h := Handler(manager.New(manager.Options{NoDisk: true}))
	if w := post(h, "/status", "", api.Req{}); w.Code != http.StatusConflict {
		t.Fatalf("a client without a version was served: %d", w.Code)
	}
	if w := post(h, "/status", "1", api.Req{}); w.Code != http.StatusConflict {
		t.Fatalf("an old client was served: %d", w.Code)
	}
	if w := post(h, "/status", strconv.Itoa(api.Version), api.Req{}); w.Code != http.StatusOK {
		t.Fatalf("status: %d %s", w.Code, w.Body)
	}
	if w := post(h, "/nope", strconv.Itoa(api.Version), api.Req{}); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown path: %d", w.Code)
	}
	// A request with no traceable caller (no socket here) gets no one's rights.
	if w := post(h, "/acquire", strconv.Itoa(api.Version), api.Req{Kind: "cluster", Purpose: "x", Session: "you"}); w.Code != http.StatusBadRequest {
		t.Fatalf("an untraceable caller took a lease: %d %s", w.Code, w.Body)
	}
}
