// Package server serves the daemon's API on a unix socket. It reads each caller's PID from the
// socket, checks the protocol version, and hands the request to the manager.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/manager"
	"agentrm/internal/store"
	"agentrm/internal/sys"
)

func Serve(ctx context.Context, m *manager.Manager) error {
	sock := store.SockPath()
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	_ = os.Chmod(sock, 0o600) // only this user can talk to the daemon
	srv := &http.Server{Handler: Handler(m), ConnContext: sys.WithPeer, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func Handler(m *manager.Manager) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if v := r.Header.Get(api.VersionHeader); v != strconv.Itoa(api.Version) {
			writeJSON(w, http.StatusConflict, api.Error{Error: fmt.Sprintf(
				"this client speaks API version %s and the daemon speaks %d. Restart AgentRM, or run: launchctl kickstart -k gui/$(id -u)/dev.agentrm.daemon",
				orNone(v), api.Version)})
			return
		}
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, api.Error{Error: "POST only"})
			return
		}
		var req api.Req
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, api.Error{Error: "bad request: " + err.Error()})
			return
		}
		resp, err := m.Handle(r.URL.Path, sys.PeerFrom(r.Context()), req)
		var na *manager.ErrNeedsApproval
		switch {
		case errors.As(err, &na):
			writeJSON(w, http.StatusAccepted, api.Error{Error: na.Error(), ApprovalID: na.Approval.ID})
		case err != nil:
			writeJSON(w, http.StatusBadRequest, api.Error{Error: err.Error()})
		default:
			writeJSON(w, http.StatusOK, resp)
		}
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func orNone(v string) string {
	if v == "" {
		return "none"
	}
	return v
}
