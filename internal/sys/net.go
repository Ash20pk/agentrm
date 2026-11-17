package sys

import (
	"context"
	"net"
)

type peerKey struct{}

// WithPeer stores the connection's peer PID in the request context (http.Server.ConnContext).
func WithPeer(ctx context.Context, c net.Conn) context.Context {
	return context.WithValue(ctx, peerKey{}, PeerPID(c))
}

func PeerFrom(ctx context.Context) int {
	pid, _ := ctx.Value(peerKey{}).(int)
	return pid
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
