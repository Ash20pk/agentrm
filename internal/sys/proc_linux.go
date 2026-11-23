package sys

// Linux: everything comes from /proc, with no external command.

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const clockTicks = 100 // USER_HZ, fixed at 100 on every Linux ABI that matters

var bootTime = sync.OnceValue(func() time.Time {
	b, _ := os.ReadFile("/proc/stat")
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "btime "); ok {
			if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
				return time.Unix(n, 0)
			}
		}
	}
	return time.Time{}
})

var pageMB = float64(os.Getpagesize()) / (1 << 20)

// statFields reads /proc/<pid>/stat: the fields after the command name (which may hold spaces
// and parentheses).
func statFields(pid int) []string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return nil
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return nil
	}
	return strings.Fields(string(b[i+1:])) // [0] is state, [1] ppid, ...
}

func readProc(pid int, now time.Time) *Proc {
	f := statFields(pid)
	if len(f) < 22 {
		return nil
	}
	ppid, _ := strconv.Atoi(f[1])
	utime, _ := strconv.ParseFloat(f[11], 64)
	stime, _ := strconv.ParseFloat(f[12], 64)
	startTicks, _ := strconv.ParseFloat(f[19], 64)
	rssPages, _ := strconv.ParseFloat(f[21], 64)
	p := &Proc{PID: pid, PPID: ppid, RSSMB: rssPages * pageMB}
	if bt := bootTime(); !bt.IsZero() {
		p.Start = bt.Add(time.Duration(startTicks / clockTicks * float64(time.Second))).Truncate(time.Second)
		if age := now.Sub(p.Start).Seconds(); age > 0 {
			p.CPU = 100 * (utime + stime) / clockTicks / age // like ps: the average over its life
		}
	}
	cmd, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	args := strings.Fields(strings.ReplaceAll(strings.TrimRight(string(cmd), "\x00"), "\x00", " "))
	if len(args) == 0 {
		comm, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
		args = []string{"[" + strings.TrimSpace(string(comm)) + "]"} // a kernel thread
	}
	p.Exe, p.Args = args[0], strings.Join(args, " ")
	p.Path = ExePath(pid)
	return p
}

// ExePath returns the real path of the executable pid runs (/proc/<pid>/exe), which the
// process cannot choose, unlike its argv[0]. "" when the process is gone or not readable.
func ExePath(pid int) string {
	path, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(path, " (deleted)")
}

// ListProcs reads the whole process table from /proc.
func ListProcs() map[int]*Proc {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	now := time.Now()
	procs := map[int]*Proc{}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if p := readProc(pid, now); p != nil {
			procs[pid] = p
		}
	}
	// A container has no launchd; its PID 1 is whatever started it. Either way the table
	// must hold PID 1 and this process to be whole.
	if !CompleteTable(procs, os.Getpid()) {
		return nil
	}
	return procs
}

// Cwds returns the working directory of each pid.
func Cwds(pids []int) map[int]string {
	out := map[int]string{}
	for _, pid := range pids {
		if d, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/cwd"); err == nil {
			out[pid] = d
		}
	}
	return out
}

// ReadProcInfo reads a process's argv and environment from /proc. Readable for any process
// of this user (there is no exception like macOS's platform binaries).
func ReadProcInfo(pid int) (ProcInfo, bool) {
	dir := "/proc/" + strconv.Itoa(pid)
	cmd, err := os.ReadFile(dir + "/cmdline")
	if err != nil {
		return ProcInfo{}, false
	}
	var info ProcInfo
	if len(cmd) > 0 {
		// Each argument ends in a NUL; an empty argument is two NULs in a row and is kept.
		info.Argv = strings.Split(strings.TrimSuffix(string(cmd), "\x00"), "\x00")
	}
	env, err := os.ReadFile(dir + "/environ")
	if err != nil {
		return info, true
	}
	for _, kv := range strings.Split(string(env), "\x00") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			info.addEnv(k, v)
		}
	}
	info.finish()
	return info, true
}

// Footprint is what a process holds in memory, swapped-out pages included.
func Footprint(pid int) (float64, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0, false
	}
	kb := 0.0
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "VmRSS:") || strings.HasPrefix(l, "VmSwap:") {
			if f := strings.Fields(l); len(f) >= 2 {
				v, _ := strconv.ParseFloat(f[1], 64)
				kb += v
			}
		}
	}
	return kb / 1024, true
}

// QuickProc reads one process (the PATH shim's fast path).
func QuickProc(pid int) *Proc { return readProc(pid, time.Now()) }

// QuickAncestry walks up from pid to the nearest agent, and says whether a terminal or desktop
// app is above it.
func QuickAncestry(pid int) (agentPID int, agent string, gui bool) {
	for i := 0; i < 64 && pid > 1; i++ {
		p := QuickProc(pid)
		if p == nil {
			return 0, "", false
		}
		if k := AgentKind(p); k != "" {
			return p.PID, k, false
		}
		if isGUIApp(p.Path) {
			gui = true
		}
		pid = p.PPID
	}
	return 0, "", gui
}

// ---- memory ----

func meminfo() map[string]float64 {
	out := map[string]float64{}
	b, _ := os.ReadFile("/proc/meminfo")
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) >= 2 {
			v, _ := strconv.ParseFloat(f[1], 64)
			out[strings.TrimSuffix(f[0], ":")] = v / 1024 // kB to MB
		}
	}
	return out
}

// ReadFast reads memory, swap, pressure, load and disk from /proc.
func ReadFast() MemStats {
	mi := meminfo()
	m := MemStats{TotalMB: mi["MemTotal"], AvailableMB: mi["MemAvailable"], SwapTotalMB: mi["SwapTotal"],
		SwapUsedMB: mi["SwapTotal"] - mi["SwapFree"]}
	m.PressureLevel = PressureLevel()
	m.Load1, m.Cores = LoadAvg()
	m.DiskFreeGB = DiskFreeGB("/")
	if m.TotalMB > 0 {
		m.FreePercent = int(100 * m.AvailableMB / m.TotalMB)
	}
	return m
}

// ReadMem is ReadFast plus the cumulative swap-out count (for the swap rate).
func ReadMem() MemStats {
	m := ReadFast()
	b, _ := os.ReadFile("/proc/vmstat")
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "pswpout "); ok {
			n, _ := strconv.ParseFloat(v, 64)
			m.Swapouts = n * pageMB
		}
	}
	return m
}

// PressureLevel maps the kernel's pressure stall information (PSI) onto the macOS levels: 4
// critical when every task stalled on memory over a tenth of the last ten seconds, 2 warning
// when some did, 1 otherwise (and when PSI is not available).
func PressureLevel() int {
	b, err := os.ReadFile("/proc/pressure/memory")
	if err != nil {
		return 1
	}
	avg10 := func(line string) float64 {
		for _, f := range strings.Fields(line) {
			if v, ok := strings.CutPrefix(f, "avg10="); ok {
				x, _ := strconv.ParseFloat(v, 64)
				return x
			}
		}
		return 0
	}
	some, full := 0.0, 0.0
	for _, l := range strings.Split(string(b), "\n") {
		switch {
		case strings.HasPrefix(l, "some "):
			some = avg10(l)
		case strings.HasPrefix(l, "full "):
			full = avg10(l)
		}
	}
	switch {
	case full >= 10:
		return 4
	case some >= 10:
		return 2
	}
	return 1
}

func LoadAvg() (float64, int) {
	b, _ := os.ReadFile("/proc/loadavg")
	if f := strings.Fields(string(b)); len(f) > 0 {
		v, _ := strconv.ParseFloat(f[0], 64)
		return v, runtime.NumCPU()
	}
	return 0, runtime.NumCPU()
}

// ---- sockets ----

// PeerPID is the process on the other end of a unix socket (SO_PEERCRED).
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
		if cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED); err == nil {
			pid = int(cred.Pid)
		}
	})
	return pid
}

type tcpRow struct {
	local, remote string // "ip:port"
	lport         string
	state         string // 01 established, 0A listen
	inode         string
}

func tcpTable() []tcpRow {
	var rows []tcpRow
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for i, l := range strings.Split(string(b), "\n") {
			fs := strings.Fields(l)
			if i == 0 || len(fs) < 10 {
				continue
			}
			port := func(addr string) string {
				if j := strings.LastIndexByte(addr, ':'); j >= 0 {
					n, _ := strconv.ParseInt(addr[j+1:], 16, 32)
					return strconv.Itoa(int(n))
				}
				return ""
			}
			rows = append(rows, tcpRow{local: fs[1], remote: fs[2], lport: port(fs[1]), state: fs[3], inode: fs[9]})
		}
	}
	return rows
}

// EstablishedPorts lists every local or remote port in an established TCP connection.
func EstablishedPorts() map[string]bool {
	ports := map[string]bool{}
	for _, r := range tcpTable() {
		if r.state == "01" {
			ports[r.lport] = true
			if j := strings.LastIndexByte(r.remote, ':'); j >= 0 {
				n, _ := strconv.ParseInt(r.remote[j+1:], 16, 32)
				ports[strconv.Itoa(int(n))] = true
			}
		}
	}
	return ports
}

// ListeningPorts maps each pid to the TCP ports it listens on, through the sockets' inodes.
func ListeningPorts() map[int][]string {
	byInode := map[string]string{}
	for _, r := range tcpTable() {
		if r.state == "0A" {
			byInode[r.inode] = r.lport
		}
	}
	out := map[int][]string{}
	if len(byInode) == 0 {
		return out
	}
	fds, _ := filepath.Glob("/proc/[0-9]*/fd/*")
	for _, fd := range fds {
		link, err := os.Readlink(fd)
		if err != nil || !strings.HasPrefix(link, "socket:[") {
			continue
		}
		port, ok := byInode[strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")]
		if !ok {
			continue
		}
		pid, _ := strconv.Atoi(strings.Split(fd, "/")[2])
		if !contains(out[pid], port) {
			out[pid] = append(out[pid], port)
		}
	}
	return out
}

// ConnTuples returns the established connections to each of ports, as "local remote" pairs,
// including ones the kernel accepted that no process has taken yet.
func ConnTuples(ports []string) map[string]map[string]bool {
	want := map[string]bool{}
	for _, p := range ports {
		want[p] = true
	}
	out := map[string]map[string]bool{}
	for _, r := range tcpTable() {
		if r.state != "01" || !want[r.lport] {
			continue
		}
		if out[r.lport] == nil {
			out[r.lport] = map[string]bool{}
		}
		out[r.lport][r.local+" "+r.remote] = true
	}
	return out
}

// ---- exits ----

// ExitWatcher reports processes exiting the moment they do: a pidfd per watched process,
// which becomes readable when it exits.
type ExitWatcher struct {
	C chan int

	mu      sync.Mutex
	watched map[int]bool
}

func NewExitWatcher() (*ExitWatcher, error) {
	if _, err := unix.PidfdOpen(os.Getpid(), 0); err != nil {
		return nil, errors.New("pidfd_open is not available (Linux 5.3+ needed)")
	}
	return &ExitWatcher{C: make(chan int, 64), watched: map[int]bool{}}, nil
}

func (w *ExitWatcher) Watch(pid int) {
	if pid <= 1 {
		return
	}
	w.mu.Lock()
	if w.watched[pid] {
		w.mu.Unlock()
		return
	}
	w.watched[pid] = true
	w.mu.Unlock()
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		w.done(pid)
		return
	}
	go func() {
		defer unix.Close(fd)
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		for {
			if _, err := unix.Poll(fds, -1); err != nil && errors.Is(err, syscall.EINTR) {
				continue
			}
			break
		}
		w.done(pid)
	}()
}

func (w *ExitWatcher) done(pid int) {
	w.mu.Lock()
	delete(w.watched, pid)
	w.mu.Unlock()
	select {
	case w.C <- pid:
	default:
	}
}
