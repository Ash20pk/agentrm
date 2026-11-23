package discover

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// shortDir is a temp dir short enough for a unix socket path (104 bytes on macOS).
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "arm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func listen(t *testing.T, path string, serve func(net.Conn)) net.Listener {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serve(c)
		}
	}()
	return ln
}

func TestProbeSocketTellsDownFromHung(t *testing.T) {
	d := shortDir(t)

	// No socket at all: Docker is not running.
	if s := probeSocket(filepath.Join(d, "none.sock"), time.Second); s != EngineDown {
		t.Fatalf("missing socket: %v", s)
	}

	// A socket file nobody listens on (Docker quit and left it behind): down.
	stale := filepath.Join(d, "stale.sock")
	ln, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("socket file should remain: %v", err)
	}
	if s := probeSocket(stale, time.Second); s != EngineDown {
		t.Fatalf("socket with no listener: %v", s)
	}

	// An engine that answers.
	up := filepath.Join(d, "up.sock")
	listen(t, up, func(c net.Conn) {
		defer c.Close()
		r := bufio.NewReader(c)
		for {
			l, err := r.ReadString('\n')
			if err != nil || l == "\r\n" {
				break
			}
		}
		c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nOK"))
	})
	if s := probeSocket(up, time.Second); s != EngineUp {
		t.Fatalf("answering engine: %v", s)
	}

	// An engine that accepts and never answers: hung, not down.
	hung := filepath.Join(d, "hung.sock")
	listen(t, hung, func(c net.Conn) { time.Sleep(3 * time.Second); c.Close() })
	start := time.Now()
	if s := probeSocket(hung, 300*time.Millisecond); s != EngineHung {
		t.Fatalf("silent engine: %v", s)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("the probe did not respect its timeout")
	}

	// A starting engine answers with an error: not up yet.
	starting := filepath.Join(d, "start.sock")
	listen(t, starting, func(c net.Conn) {
		defer c.Close()
		c.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
	})
	if s := probeSocket(starting, time.Second); s != EngineHung {
		t.Fatalf("starting engine: %v", s)
	}
}

func TestDockerIsEmptyWhenTheEngineIsDown(t *testing.T) {
	d := shortDir(t)
	old := dockerSocket
	dockerSocket = func() string { return filepath.Join(d, "gone.sock") }
	t.Cleanup(func() { dockerSocket = old })
	rs, err := Docker()
	if err != nil || len(rs) != 0 {
		t.Fatalf("a stopped engine must read as nothing running, certainly: %v %v", rs, err)
	}
}

func TestResolveDockerSocketFollowsTheCurrentContext(t *testing.T) {
	d := shortDir(t)
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_CONFIG", d)
	os.WriteFile(filepath.Join(d, "config.json"), []byte(`{"currentContext":"desktop-linux"}`), 0o644)
	// The CLI stores a context's metadata under the SHA-256 of its name.
	meta := filepath.Join(d, "contexts", "meta", "fe9c6bd7a66301f49ca9b6a70b217107cd1284598bfc254700c989b916da791e")
	os.MkdirAll(meta, 0o755)
	os.WriteFile(filepath.Join(meta, "meta.json"),
		[]byte(`{"Name":"desktop-linux","Endpoints":{"docker":{"Host":"unix:///Users/x/.docker/run/docker.sock"}}}`), 0o644)
	if got := resolveDockerSocket(); got != "/Users/x/.docker/run/docker.sock" {
		t.Fatalf("context socket: %q", got)
	}
	t.Setenv("DOCKER_CONTEXT", "default")
	if got := resolveDockerSocket(); got != "/var/run/docker.sock" {
		t.Fatalf("default context: %q", got)
	}
	t.Setenv("DOCKER_HOST", "tcp://10.0.0.2:2375")
	if got := resolveDockerSocket(); got != "" {
		t.Fatalf("a tcp endpoint cannot be probed as a socket: %q", got)
	}
}
