package sys

import (
	"bytes"
	"encoding/binary"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// QuickProc reads one process with sysctl: no ps, no fork. Used by the PATH shim, which must
// add almost nothing to every command it passes through.
func QuickProc(pid int) *Proc {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp.Proc.P_pid == 0 {
		return nil
	}
	p := &Proc{PID: pid, PPID: int(kp.Eproc.Ppid)}
	if st := kp.Proc.P_starttime; st.Sec > 0 {
		p.Start = time.Unix(st.Sec, 0) // to the second, like ps's lstart
	}
	comm := string(bytes.TrimRight(kp.Proc.P_comm[:], "\x00"))
	p.Exe, p.Args = comm, comm
	// kern.procargs2: int32 argc, the exec path, NUL padding, then argc NUL-terminated args.
	if b, err := unix.SysctlRaw("kern.procargs2", pid); err == nil && len(b) > 4 {
		argc := int(binary.LittleEndian.Uint32(b[:4]))
		rest := b[4:]
		if i := bytes.IndexByte(rest, 0); i > 0 {
			p.Exe = string(rest[:i])
			rest = rest[i:]
		}
		rest = bytes.TrimLeft(rest, "\x00")
		var args []string
		for len(args) < argc && len(rest) > 0 {
			i := bytes.IndexByte(rest, 0)
			if i < 0 {
				args = append(args, string(rest))
				break
			}
			args = append(args, string(rest[:i]))
			rest = rest[i+1:]
		}
		if len(args) > 0 {
			p.Args = strings.Join(args, " ")
		}
	}
	return p
}

// QuickAncestry walks up from pid with sysctl and reports the nearest agent and whether a GUI
// app is above it (no agent above means a person if a GUI app is, nobody otherwise).
func QuickAncestry(pid int) (agentPID int, agent string, gui bool) {
	for i := 0; i < 64 && pid > 1; i++ {
		p := QuickProc(pid)
		if p == nil {
			return 0, "", false
		}
		if k := AgentKind(p); k != "" {
			return p.PID, k, false
		}
		if strings.Contains(p.Exe, ".app/Contents/") {
			gui = true
		}
		pid = p.PPID
	}
	return 0, "", gui
}
