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
// environment, each NUL-terminated.
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
	var info ProcInfo
	for len(rest) > 0 {
		j := bytes.IndexByte(rest, 0)
		if j <= 0 {
			break // the end of the strings area is a run of NULs
		}
		s := string(rest[:j])
		rest = rest[j+1:]
		if len(info.Argv) < argc {
			info.Argv = append(info.Argv, s)
			continue
		}
		k, v, ok := strings.Cut(s, "=")
		if !ok {
			continue
		}
		info.addEnv(k, v)
	}
	return info, true
}
