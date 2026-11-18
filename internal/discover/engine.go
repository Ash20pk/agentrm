package discover

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Whether the Docker engine is running is asked of its socket, not of the docker CLI. The CLI
// cannot tell "Docker is gone" from "Docker is slow": both look like a command that fails or
// hangs. The socket can: no socket, or a connection refused, means no engine, and so none of
// its containers, for certain. An engine that accepts the connection and does not answer is
// hung, and nothing is concluded about its containers.

// EngineState is what the Docker engine's socket says.
type EngineState int

const (
	EngineUnknown EngineState = iota // the endpoint is not a local socket (tcp, ssh): ask the CLI
	EngineUp                         // answered /_ping
	EngineDown                       // no socket, or nothing listening on it
	EngineHung                       // accepted the connection, but did not answer in time
)

func (s EngineState) String() string {
	return [...]string{"unknown", "up", "down", "not responding"}[s]
}

// ErrEngineHung is a Docker engine that is there but does not answer.
var ErrEngineHung = errors.New("the Docker engine is not responding")

// dockerSocket finds the socket the docker CLI would use. A variable so tests can point it at
// their own.
var dockerSocket = resolveDockerSocket

// probeTimeout bounds the whole check: connect, ask, read the answer.
var probeTimeout = 3 * time.Second

// DockerEngine says whether the Docker engine the CLI talks to is running, and which socket that
// is.
func DockerEngine() (EngineState, string) {
	sock := dockerSocket()
	if sock == "" {
		return EngineUnknown, ""
	}
	return probeSocket(sock, probeTimeout), sock
}

// probeSocket asks a Docker engine's socket for /_ping.
func probeSocket(path string, timeout time.Duration) EngineState {
	if _, err := os.Stat(path); err != nil {
		return EngineDown // follows symlinks: /var/run/docker.sock pointing at a removed socket
	}
	deadline := time.Now().Add(timeout)
	conn, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT) {
			return EngineDown
		}
		return EngineHung // a full listen queue, or a dial timeout: something is there
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline)
	if _, err := fmt.Fprint(conn, "GET /_ping HTTP/1.0\r\nHost: docker\r\n\r\n"); err != nil {
		return EngineHung
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		if errors.Is(err, syscall.ECONNRESET) && line == "" {
			return EngineDown // the listener went away while we asked
		}
		return EngineHung
	}
	// "HTTP/1.1 200 OK". Anything else (a starting engine answers 500 or 502) is not up yet.
	if f := strings.Fields(line); len(f) >= 2 && strings.HasPrefix(f[0], "HTTP/") && f[1] == "200" {
		return EngineUp
	}
	return EngineHung
}

// resolveDockerSocket follows the docker CLI's own rules: DOCKER_HOST, then DOCKER_CONTEXT, then
// the current context in ~/.docker/config.json, then the default socket. It returns "" for an
// endpoint that is not a unix socket.
func resolveDockerSocket() string {
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		return unixPath(h)
	}
	home, _ := os.UserHomeDir()
	cfgDir := os.Getenv("DOCKER_CONFIG")
	if cfgDir == "" {
		cfgDir = filepath.Join(home, ".docker")
	}
	name := os.Getenv("DOCKER_CONTEXT")
	if name == "" {
		var cfg struct {
			CurrentContext string `json:"currentContext"`
		}
		if b, err := os.ReadFile(filepath.Join(cfgDir, "config.json")); err == nil {
			_ = json.Unmarshal(b, &cfg)
		}
		name = cfg.CurrentContext
	}
	if name == "" || name == "default" {
		return "/var/run/docker.sock"
	}
	sum := sha256.Sum256([]byte(name))
	b, err := os.ReadFile(filepath.Join(cfgDir, "contexts", "meta", hex.EncodeToString(sum[:]), "meta.json"))
	if err != nil {
		return ""
	}
	var meta struct {
		Endpoints map[string]struct {
			Host string `json:"Host"`
		} `json:"Endpoints"`
	}
	if json.Unmarshal(b, &meta) != nil {
		return ""
	}
	return unixPath(meta.Endpoints["docker"].Host)
}

func unixPath(host string) string {
	p, ok := strings.CutPrefix(host, "unix://")
	if !ok {
		return ""
	}
	return p
}
