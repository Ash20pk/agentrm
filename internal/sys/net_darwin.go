package sys

import (
	"net"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ---- peer PID of a unix-socket connection (macOS LOCAL_PEERPID) ----

const solLocal, localPeerPID = 0, 0x002

func PeerPID(c net.Conn) int {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0
	}
	pid := 0
	_ = raw.Control(func(fd uintptr) {
		pid, _ = syscall.GetsockoptInt(int(fd), solLocal, localPeerPID)
	})
	return pid
}

// EstablishedPorts lists every local or remote port in an ESTABLISHED TCP connection owned by
// a process.
func EstablishedPorts() map[string]bool {
	out, _ := Run(5*time.Second, lsofPath, "-nP", "-iTCP", "-sTCP:ESTABLISHED", "-Fn")
	ports := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		if !strings.HasPrefix(l, "n") {
			continue
		}
		for _, side := range strings.Split(l[1:], "->") {
			if i := strings.LastIndex(side, ":"); i >= 0 {
				ports[side[i+1:]] = true
			}
		}
	}
	return ports
}

// ListeningPorts maps each pid to the TCP ports it listens on.
func ListeningPorts() map[int][]string {
	out, err := Run(5*time.Second, lsofPath, "-nP", "-iTCP", "-sTCP:LISTEN", "-Fpn")
	ports := map[int][]string{}
	if err != nil && out == "" {
		return ports
	}
	pid := 0
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(l, "p"):
			pid, _ = strconv.Atoi(l[1:])
		case strings.HasPrefix(l, "n") && pid != 0:
			addr := l[1:]
			port := addr[strings.LastIndex(addr, ":")+1:]
			if !contains(ports[pid], port) {
				ports[pid] = append(ports[pid], port)
			}
		}
	}
	return ports
}

// ConnTuples returns, for each of ports, the established TCP connections to it as
// "local foreign" pairs. It includes connections the kernel accepted that no process owns yet:
// a frozen server cannot accept, so a connection to it waits there, invisible to lsof. A new
// pair since the last look is someone trying to use the port.
func ConnTuples(ports []string) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	want := map[string]bool{}
	for _, p := range ports {
		want[p] = true
	}
	if len(want) == 0 {
		return out
	}
	s, _ := Run(5*time.Second, netstatPath, "-an", "-p", "tcp")
	for _, l := range strings.Split(s, "\n") {
		f := strings.Fields(l)
		if len(f) < 6 || f[5] != "ESTABLISHED" {
			continue
		}
		local := f[3]
		i := strings.LastIndex(local, ".")
		if i < 0 || !want[local[i+1:]] {
			continue
		}
		port := local[i+1:]
		if out[port] == nil {
			out[port] = map[string]bool{}
		}
		out[port][local+" "+f[4]] = true
	}
	return out
}
