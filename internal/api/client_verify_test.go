package api

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func shortDir(t *testing.T) string {
	d, err := os.MkdirTemp("", "api")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// A socket served by this same executable is AgentRM's daemon.
func TestVerifyDaemonAcceptsItsOwnBinary(t *testing.T) {
	sock := filepath.Join(shortDir(t), "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			time.Sleep(time.Second)
			c.Close()
		}
	}()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := verifyDaemon(c); err != nil {
		t.Fatal(err)
	}
}

// Something else listening in the socket's place is refused before anything is sent.
func TestVerifyDaemonRefusesAnImpostor(t *testing.T) {
	nc, err := exec.LookPath("nc")
	if err != nil {
		t.Skip("no nc")
	}
	sock := filepath.Join(shortDir(t), "s")
	cmd := exec.Command(nc, "-lU", sock)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	var c net.Conn
	for i := 0; i < 50; i++ {
		if c, err = net.Dial("unix", sock); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := verifyDaemon(c); !errors.Is(err, ErrNotDaemon) {
		t.Fatalf("an impostor passed: %v", err)
	}
	t.Setenv("AGENTRM_SOCK", sock)
	if err := CallDaemon(2*time.Second, "/vault/answer", Req{}, nil); !errors.Is(err, ErrNotDaemon) {
		t.Fatalf("CallDaemon sent to an impostor: %v", err)
	}
}
