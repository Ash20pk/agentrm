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
	"sync"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/manager"
	"agentrm/internal/store"
	"agentrm/internal/sys"
)

// Serve takes the daemon lock and serves the API until ctx ends. A second daemon gets an
// error wrapping store.ErrDaemonRunning and leaves the first one's socket alone.
func Serve(ctx context.Context, m *manager.Manager) error {
	lock, err := store.LockDaemon()
	if err != nil {
		return err
	}
	defer lock.Release()
	return ServeLocked(ctx, m, lock)
}

// ServeLocked serves the API for a daemon that already holds the lock (taken before the
// manager starts, so a second daemon never sweeps or writes the state either).
func ServeLocked(ctx context.Context, m *manager.Manager, lock *store.DaemonLock) error {
	if lock == nil {
		return errors.New("serve: the daemon lock is not held")
	}
	sock := store.SockPath()
	// Only the lock holder removes the socket: it is a dead daemon's, or none.
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	_ = os.Chmod(sock, 0o600) // only this user can talk to the daemon
	// IdleTimeout: a connection kept open between requests is how a process outlives its
	// identity check (see sys.PeerFrom); none needs to idle for long.
	fw := newFreezeWatch(ctx)
	connCtx := func(ctx context.Context, c net.Conn) context.Context {
		ctx = sys.WithPeer(ctx, c)
		if fw.justThawed() {
			ctx = sys.MarkSuspect(ctx)
		}
		return ctx
	}
	srv := &http.Server{Handler: Handler(m), ConnContext: connCtx, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
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
		resp, err := m.HandleCtx(r.Context(), r.URL.Path, sys.PeerFrom(r.Context()), req)
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

// freezeWatch notices the daemon having been frozen (kill -STOP, or the Mac asleep): a beat
// every half second that comes late. Connections accepted just after are suspect (see
// sys.PeerSuspect).
type freezeWatch struct {
	mu       sync.Mutex
	last     time.Time // the last beat (monotonic)
	thawedAt time.Time // when a late beat was last seen
}

const (
	freezeBeat  = 500 * time.Millisecond
	freezeGap   = 2 * time.Second // a beat this late means the process was not running
	freezeAfter = 2 * time.Second // how long connections stay suspect after
)

func newFreezeWatch(ctx context.Context) *freezeWatch {
	fw := &freezeWatch{last: time.Now()}
	go func() {
		t := time.NewTicker(freezeBeat)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				fw.beat(time.Now())
			}
		}
	}()
	return fw
}

func (fw *freezeWatch) beat(now time.Time) {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if now.Sub(fw.last) > freezeGap {
		fw.thawedAt = now
	}
	fw.last = now
}

// justThawed: the process was frozen and has just resumed, whether or not the beat has run
// since (accepting may come first).
func (fw *freezeWatch) justThawed() bool {
	now := time.Now()
	fw.mu.Lock()
	defer fw.mu.Unlock()
	return now.Sub(fw.last) > freezeGap || (!fw.thawedAt.IsZero() && now.Sub(fw.thawedAt) < freezeAfter)
}
