package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Resource kinds the sweeper knows how to find and stop.
const (
	RKCluster    = "kind-cluster"
	RKContainers = "containers"
	RKSimulator  = "simulator"
	RKGradle     = "gradle-daemon"
	RKServer     = "server"
)

type Resource struct {
	ID          string     `json:"id"`
	Kind        string     `json:"kind"`
	Name        string     `json:"name"`
	Detail      string     `json:"detail"`
	MemMB       float64    `json:"mem_mb"`
	Containers  []string   `json:"containers,omitempty"`
	PID         int        `json:"pid,omitempty"`
	UDID        string     `json:"udid,omitempty"`
	FirstSeen   time.Time  `json:"first_seen"`
	LeaseID     string     `json:"lease_id,omitempty"`
	OrphanSince *time.Time `json:"orphan_since,omitempty"`
	Pinned      bool       `json:"pinned,omitempty"`
	Ports       []string   `json:"ports,omitempty"`       // host TCP ports it listens on or publishes
	CPU         float64    `json:"cpu,omitempty"`         // percent, where measured
	LastActive  time.Time  `json:"last_active,omitempty"` // last sign of use seen by the sweeper
	StartedBy   string     `json:"started_by,omitempty"`  // session that started it, when known
	PausedAt    *time.Time `json:"paused_at,omitempty"`   // frozen since then, not yet stopped
	Urgent      bool       `json:"urgent,omitempty"`      // a blocked request needs its memory: stop, do not freeze
}

func run(name string, args ...string) (string, error) {
	ctxCmd := exec.Command(name, args...)
	done := make(chan struct{})
	var out []byte
	var err error
	go func() { out, err = ctxCmd.Output(); close(done) }()
	select {
	case <-done:
		return string(out), err
	case <-time.After(20 * time.Second):
		_ = ctxCmd.Process.Kill()
		return "", fmt.Errorf("%s timed out", name)
	}
}

func discover(cfg Config) []*Resource {
	var res []*Resource
	res = append(res, discoverDocker()...)
	res = append(res, discoverSimulators(cfg)...)
	res = append(res, discoverProcesses(cfg)...)
	sort.Slice(res, func(i, j int) bool { return res[i].ID < res[j].ID })
	return res
}

// hostPortRe pulls the host side out of docker's "0.0.0.0:5432->5432/tcp".
var hostPortRe = regexp.MustCompile(`:(\d+)->`)

var memUsageRe = regexp.MustCompile(`^([\d.]+)\s*([KMG]i?B|B)`)

func parseMem(s string) float64 {
	mm := memUsageRe.FindStringSubmatch(strings.TrimSpace(s))
	if mm == nil {
		return 0
	}
	v, _ := strconv.ParseFloat(mm[1], 64)
	switch mm[2] {
	case "GiB", "GB":
		return v * 1024
	case "MiB", "MB":
		return v
	case "KiB", "KB":
		return v / 1024
	}
	return v / 1048576
}

func discoverDocker() []*Resource {
	out, err := run("docker", "ps", "--format",
		`{{.Names}}	{{.Label "io.x-k8s.kind.cluster"}}	{{.Label "com.docker.compose.project"}}	{{.Label "com.supabase.cli.project"}}	{{.Image}}	{{.Ports}}`)
	if err != nil {
		return nil
	}
	mem := map[string]float64{}
	if s, err := run("docker", "stats", "--no-stream", "--format", "{{.Name}}\t{{.MemUsage}}"); err == nil {
		for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
			f := strings.SplitN(l, "\t", 2)
			if len(f) == 2 {
				mem[f[0]] = parseMem(f[1])
			}
		}
	}
	groups := map[string]*Resource{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(l, "\t")
		if len(f) < 5 || f[0] == "" {
			continue
		}
		name, kindCluster, compose, supa, image := f[0], f[1], f[2], f[3], f[4]
		ports := ""
		if len(f) > 5 {
			ports = f[5]
		}
		var id, kind, gname string
		switch {
		case kindCluster != "":
			id, kind, gname = "kind:"+kindCluster, RKCluster, kindCluster
		case supa != "":
			id, kind, gname = "compose:supabase-"+supa, RKContainers, "supabase "+supa
		case compose != "":
			id, kind, gname = "compose:"+compose, RKContainers, compose
		default:
			id, kind, gname = "docker:"+name, RKContainers, name+" ("+image+")"
		}
		g := groups[id]
		if g == nil {
			g = &Resource{ID: id, Kind: kind, Name: gname}
			groups[id] = g
		}
		g.Containers = append(g.Containers, name)
		g.MemMB += mem[name]
		for _, hp := range hostPortRe.FindAllStringSubmatch(ports, -1) {
			if !contains(g.Ports, hp[1]) {
				g.Ports = append(g.Ports, hp[1])
			}
		}
	}
	var res []*Resource
	for _, g := range groups {
		g.Detail = fmt.Sprintf("%d container(s)", len(g.Containers))
		res = append(res, g)
	}
	return res
}

func discoverSimulators(cfg Config) []*Resource {
	out, err := run("xcrun", "simctl", "list", "devices", "booted", "-j")
	if err != nil {
		return nil
	}
	var parsed struct {
		Devices map[string][]struct {
			UDID  string `json:"udid"`
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"devices"`
	}
	if json.Unmarshal([]byte(out), &parsed) != nil {
		return nil
	}
	var res []*Resource
	for runtime, devs := range parsed.Devices {
		for _, d := range devs {
			if d.State != "Booted" {
				continue
			}
			rt := runtime[strings.LastIndex(runtime, ".")+1:]
			res = append(res, &Resource{ID: "sim:" + d.UDID, Kind: RKSimulator, Name: d.Name,
				Detail: rt, UDID: d.UDID, MemMB: cfg.KindMemGB["simulator"] * 1024})
		}
	}
	return res
}

type proc struct {
	pid, ppid int
	rssMB     float64
	cpu       float64
	exe, args string
}

func listProcs() map[int]*proc {
	out, err := run("ps", "-axww", "-o", "pid=,ppid=,rss=,%cpu=,args=")
	if err != nil {
		return nil
	}
	procs := map[int]*proc{}
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) < 5 {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		ppid, _ := strconv.Atoi(f[1])
		rss, _ := strconv.ParseFloat(f[2], 64)
		cpu, _ := strconv.ParseFloat(f[3], 64)
		args := strings.Join(f[4:], " ")
		procs[pid] = &proc{pid: pid, ppid: ppid, rssMB: rss / 1024, cpu: cpu, exe: f[4], args: args}
	}
	return procs
}

func discoverProcesses(cfg Config) []*Resource {
	procs := procCache.get()
	var res []*Resource
	for _, p := range procs {
		if strings.Contains(p.args, "org.gradle.launcher.daemon") || strings.Contains(p.args, "GradleDaemon") {
			res = append(res, &Resource{ID: fmt.Sprintf("gradle:%d", p.pid), Kind: RKGradle,
				Name: "Gradle daemon", Detail: maxHeap(p.args), PID: p.pid, MemMB: p.rssMB, CPU: p.cpu})
		} else if strings.Contains(p.args, "KotlinCompileDaemon") {
			res = append(res, &Resource{ID: fmt.Sprintf("gradle:%d", p.pid), Kind: RKGradle,
				Name: "Kotlin daemon", Detail: maxHeap(p.args), PID: p.pid, MemMB: p.rssMB, CPU: p.cpu})
		}
	}
	// Listening dev servers.
	out, err := run("lsof", "-nP", "-iTCP", "-sTCP:LISTEN", "-Fpn")
	if err != nil && out == "" {
		return res
	}
	ports := map[int][]string{}
	pid := 0
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "p") {
			pid, _ = strconv.Atoi(l[1:])
		} else if strings.HasPrefix(l, "n") && pid != 0 {
			addr := l[1:]
			port := addr[strings.LastIndex(addr, ":")+1:]
			if !contains(ports[pid], port) {
				ports[pid] = append(ports[pid], port)
			}
		}
	}
	for pid, ps := range ports {
		p := procs[pid]
		if p == nil || !isDevServer(p, cfg) {
			continue
		}
		res = append(res, &Resource{ID: fmt.Sprintf("srv:%d", pid), Kind: RKServer,
			Name: filepath.Base(p.exe) + " :" + strings.Join(ps, ","), Detail: trunc(p.args, 90),
			PID: pid, MemMB: p.rssMB, CPU: p.cpu, Ports: ps})
	}
	return res
}

func isDevServer(p *proc, cfg Config) bool {
	for _, pre := range []string{"/Applications/", "/System/", "/Library/", "/usr/libexec/", "/usr/sbin/", "/sbin/"} {
		if strings.HasPrefix(p.exe, pre) {
			return false
		}
	}
	low := strings.ToLower(p.args)
	for _, x := range cfg.ServerExclude {
		if strings.Contains(low, strings.ToLower(x)) {
			return false
		}
	}
	base := filepath.Base(p.exe)
	if strings.Contains(p.exe, "/go-build") {
		return true
	}
	for _, n := range cfg.ServerNames {
		if base == n || (strings.HasSuffix(n, "*") && strings.HasPrefix(base, strings.TrimSuffix(n, "*"))) {
			return true
		}
	}
	return false
}

var xmxRe = regexp.MustCompile(`-Xmx(\S+)`)

func maxHeap(args string) string {
	if mm := xmxRe.FindStringSubmatch(args); mm != nil {
		return "max heap " + mm[1]
	}
	return ""
}

// stop shuts a resource down in a way that can be undone: containers are stopped
// (not removed), simulators shut down, processes get SIGTERM.
func stop(r *Resource) error {
	switch r.Kind {
	case RKCluster, RKContainers:
		_, err := run("docker", append([]string{"stop"}, r.Containers...)...)
		return err
	case RKSimulator:
		_, err := run("xcrun", "simctl", "shutdown", r.UDID)
		return err
	case RKGradle, RKServer:
		_, err := run("kill", "-TERM", strconv.Itoa(r.PID))
		if err == nil && r.PausedAt != nil {
			// A frozen process only sees the TERM once it runs again.
			// (It may already be gone, so a failure here means nothing.)
			_, _ = run("kill", "-CONT", strconv.Itoa(r.PID))
		}
		return err
	}
	return fmt.Errorf("don't know how to stop %s", r.Kind)
}

func howToRestart(r *Resource) string {
	switch r.Kind {
	case RKCluster, RKContainers:
		return "docker start " + strings.Join(r.Containers, " ")
	case RKSimulator:
		return "xcrun simctl boot " + r.UDID
	}
	return "re-run the command that started it"
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
