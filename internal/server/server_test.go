package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/manager"
	"agentrm/internal/store"
)

func TestSecondServeRefused(t *testing.T) {
	// A short directory: a unix socket path is limited to 104 bytes on macOS.
	dir, err := os.MkdirTemp("/tmp", "arm-lock-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("AGENTRM_HOME", dir)
	t.Setenv("AGENTRM_SOCK", filepath.Join(dir, "s.sock"))
	m := manager.New(manager.Options{NoDisk: true})

	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { first <- Serve(ctx, m) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(store.SockPath()); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first daemon never listened")
		}
		time.Sleep(10 * time.Millisecond)
	}
	before, _ := os.Stat(store.SockPath())

	err = Serve(context.Background(), m)
	if !errors.Is(err, store.ErrDaemonRunning) {
		t.Fatalf("a second daemon was not refused: %v", err)
	}
	after, err := os.Stat(store.SockPath())
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("the second daemon touched the first one's socket: %v", err)
	}

	cancel()
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	// Once the first has gone, a new daemon starts.
	ctx2, cancel2 := context.WithCancel(context.Background())
	second := make(chan error, 1)
	go func() { second <- Serve(ctx2, m) }()
	time.Sleep(100 * time.Millisecond)
	cancel2()
	if err := <-second; err != nil {
		t.Fatalf("a daemon could not start after the first exited: %v", err)
	}
}

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
