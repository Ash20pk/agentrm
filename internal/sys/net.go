package sys

import (
	"context"
	"net"
)

type peerKey struct{}

type peer struct {
	pid     int
	start   uint64
	ok      bool
	suspect bool
}

// MarkSuspect marks the connection's peer as one that may not be what its PID says (see
// PeerSuspect).
func MarkSuspect(ctx context.Context) context.Context {
	p, _ := ctx.Value(peerKey{}).(peer)
	p.suspect = true
	return context.WithValue(ctx, peerKey{}, p)
}

// PeerSuspect: the connection was accepted just after the server had been frozen. A
// connection waits in the listen queue while the server is stopped (kill -STOP), and the PID
// it was made from can be given to another process before it is accepted; the start time
// read at accept is then that other process's. Such a peer is who its PID says for being
// told things, but not for acting as the AgentRM app.
func PeerSuspect(ctx context.Context) bool {
	p, _ := ctx.Value(peerKey{}).(peer)
	return p.suspect
}

// WithPeer stores the connection's peer process in the request context
// (http.Server.ConnContext): its PID, and when that process started.
func WithPeer(ctx context.Context, c net.Conn) context.Context {
	p := peer{pid: PeerPID(c)}
	p.start, p.ok = ProcStartID(p.pid)
	return context.WithValue(ctx, peerKey{}, p)
}

// PeerFrom is the PID of the process that opened the connection, or 0 when that process is
// gone. A connection outlives its process (it can be handed to a child, or kept open), and the
// PID can then be given to another process, the AgentRM app's helper for one: the request would
// pass for that process. So the PID counts only while the process holding it is the one that
// connected.
func PeerFrom(ctx context.Context) int {
	p, _ := ctx.Value(peerKey{}).(peer)
	if p.pid <= 0 || !p.ok {
		return 0
	}
	if now, ok := ProcStartID(p.pid); !ok || now != p.start {
		return 0
	}
	return p.pid
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
