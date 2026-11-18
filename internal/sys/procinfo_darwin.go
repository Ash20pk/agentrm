package sys

import (
	"bytes"
	"encoding/binary"
	"strings"

	"golang.org/x/sys/unix"
)

// ReadProcInfo reads what a process was started with: its argument vector and environment,
// from the kernel (kern.procargs2). Any process of this user can be read, except Apple's
// platform binaries, whose environment the kernel withholds; for those ok is true with no
// environment. ok is false when the process is gone or not readable.
func ReadProcInfo(pid int) (ProcInfo, bool) {
	b, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil || len(b) < 4 {
		return ProcInfo{}, false
	}
	return parseProcArgs(b)
}

// parseProcArgs reads kern.procargs2: argc, the executable path, padding, argv, then the
// environment, each NUL-terminated. An argument may be empty (two NULs in a row), so argv is
// read as exactly argc strings; the environment ends at the first empty string, which no
// variable can be. A process that rewrote its arguments (setproctitle: "postgres: checkpointer")
// reads as what it wrote, which is not a command that can be run again.
func parseProcArgs(b []byte) (ProcInfo, bool) {
	if len(b) < 4 {
		return ProcInfo{}, false
	}
	argc := int(binary.LittleEndian.Uint32(b[:4]))
	rest := b[4:]
	i := bytes.IndexByte(rest, 0)
	if i < 0 {
		return ProcInfo{}, false
	}
	rest = rest[i:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	next := func() (string, bool) {
		if len(rest) == 0 {
			return "", false
		}
		j := bytes.IndexByte(rest, 0)
		if j < 0 {
			s := string(rest)
			rest = nil
			return s, true
		}
		s := string(rest[:j])
		rest = rest[j+1:]
		return s, true
	}
	var info ProcInfo
	for len(info.Argv) < argc {
		s, ok := next()
		if !ok {
			break
		}
		info.Argv = append(info.Argv, s)
	}
	for {
		s, ok := next()
		if !ok || s == "" {
			break // the end of the environment
		}
		if k, v, ok := strings.Cut(s, "="); ok {
			info.addEnv(k, v)
		}
	}
	return info, true
}
