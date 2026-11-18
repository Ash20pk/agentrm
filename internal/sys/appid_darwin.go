package sys

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// The AgentRM app talks to the daemon through the agentrm helper bundled in it
// (Contents/Helpers/agentrm), which it runs as a child process.
const (
	appMain   = "/Contents/MacOS/AgentRM"
	appHelper = "/Contents/Helpers/agentrm"
)

// AppCaller reports whether pid is the AgentRM app: the agentrm helper of a trusted AgentRM.app,
// run by that same app's main executable. Everything is read from the kernel at the time of
// the call, not from what the processes say about themselves:
//
//   - the caller's real executable (proc_pidpath) is <bundle>/Contents/Helpers/agentrm, and
//     agrees with the process table (the PID was not reused in between);
//   - its parent's real executable is <bundle>/Contents/MacOS/AgentRM. Running the helper
//     detached, or from a shell, does not make it the app;
//   - the bundle is /Applications/AgentRM.app or the bundle this daemon itself runs from;
//   - both executables are validly signed by the team that signed this daemon. An unsigned
//     daemon (a development build) cannot check this, and trusts the paths alone.
func AppCaller(procs map[int]*Proc, pid int) bool {
	p := procs[pid]
	if p == nil || p.Path == "" {
		return false
	}
	path, ppid, ok := liveExe(pid)
	if !ok || path != p.Path {
		return false
	}
	for _, b := range appBundles() {
		if path != b+appHelper {
			continue
		}
		parent, _, ok := liveExe(ppid)
		return ok && parent == b+appMain && appSigned(b+appHelper) && appSigned(b+appMain)
	}
	return false
}

// liveExe reads a process's real executable path and parent from the kernel. A variable so that
// tests can stand in for the kernel.
var liveExe = func(pid int) (path string, ppid int, ok bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp.Proc.P_pid != int32(pid) {
		return "", 0, false
	}
	path = ExePath(pid)
	return path, int(kp.Eproc.Ppid), path != ""
}

// appBundles are the AgentRM.app bundles whose helper speaks for the app: the one in
// /Applications, and the one this daemon runs from, if it runs from one.
var appBundles = sync.OnceValue(func() []string {
	out := []string{"/Applications/AgentRM.app"}
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		if i := strings.Index(exe, ".app/Contents/"); i >= 0 && exe[:i+4] != out[0] {
			out = append(out, exe[:i+4])
		}
	}
	return out
})

// ---- code signature ----

var teamIDRe = regexp.MustCompile(`(?m)^TeamIdentifier=(\S+)`)

// selfTeam is the team that signed this daemon, or "" when it is not signed by a team.
var selfTeam = sync.OnceValue(func() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return signingTeam(exe)
})

func signingTeam(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "/usr/bin/codesign", "-dv", path).CombinedOutput()
	if m := teamIDRe.FindSubmatch(out); m != nil && string(m[1]) != "not" {
		return string(m[1])
	}
	return ""
}

// fileID is what changes when a file is replaced or rewritten: a check of the file is valid
// while its fileID is the same.
type fileID struct {
	dev, ino, size int64
	mtime, ctime   syscall.Timespec
}

func statID(path string) (fileID, bool) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return fileID{}, false
	}
	return fileID{int64(st.Dev), int64(st.Ino), st.Size, st.Mtimespec, st.Ctimespec}, true
}

var sigCache = struct {
	sync.Mutex
	m map[string]fileID // path -> the file that was verified
}{m: map[string]fileID{}}

// appSigned reports whether the file at path is validly signed by this daemon's team. The
// result is remembered while the file stays the same (device, inode, size, mtime, ctime), so
// codesign runs once per install, not once per request.
var appSigned = func(path string) bool {
	team := selfTeam()
	if team == "" {
		return true // an unsigned daemon has no team to compare with: paths only
	}
	id, ok := statID(path)
	if !ok {
		return false
	}
	sigCache.Lock()
	verified, hit := sigCache.m[path]
	sigCache.Unlock()
	if hit && verified == id {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req := `=anchor apple generic and certificate leaf[subject.OU] = "` + team + `"`
	if exec.CommandContext(ctx, "/usr/bin/codesign", "--verify", "--strict", "-R", req, path).Run() != nil {
		return false
	}
	if after, ok := statID(path); !ok || after != id {
		return false // replaced while it was being checked
	}
	sigCache.Lock()
	sigCache.m[path] = id
	sigCache.Unlock()
	return true
}
