package sys

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// The peer counts only while the process that connected still holds its PID.
func TestPeerFromChecksTheProcessIsTheSame(t *testing.T) {
	dir, err := os.MkdirTemp("", "peer")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ln, err := net.Listen("unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := net.Dial("unix", filepath.Join(dir, "s"))
		if err == nil {
			defer c.Close()
			_, _ = c.Read(make([]byte, 1))
		}
	}()
	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := WithPeer(context.Background(), c)
	if got := PeerFrom(ctx); got != os.Getpid() {
		t.Fatalf("peer %d, want this process %d", got, os.Getpid())
	}
	// The same PID, now held by a process that started at another time: nobody.
	p := ctx.Value(peerKey{}).(peer)
	p.start++
	if got := PeerFrom(context.WithValue(ctx, peerKey{}, p)); got != 0 {
		t.Fatalf("a reused PID passed as the peer: %d", got)
	}
	if got := PeerFrom(context.Background()); got != 0 {
		t.Fatalf("no peer: %d", got)
	}
}
