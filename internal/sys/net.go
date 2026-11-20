package sys

import (
	"context"
	"net"
)

type peerKey struct{}

type peer struct {
	pid   int
	start uint64
	ok    bool
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
