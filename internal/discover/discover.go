// Package discover finds what is running that AgentRM manages, and acts on it.
package discover

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"agentrm/internal/model"
	"agentrm/internal/sys"
)

// Sources, each discovered independently. If one fails, the caller keeps its last view (D4).
const (
	SrcDocker = "docker"
	SrcSimctl = "simctl"
	SrcProcs  = "procs"
	SrcOllama = "ollama"
)

type Result struct {
	Resources []*model.Resource
	Failed    map[string]error // source -> why it could not be read this time
}

// Discover lists every managed resource. procs is the current process table.
func Discover(cfg model.Config, procs map[int]*sys.Proc) Result {
	res := Result{Failed: map[string]error{}}
	add := func(src string, rs []*model.Resource, err error) {
		if err != nil {
			res.Failed[src] = err
			return
		}
		for _, r := range rs {
			r.Source = src
		}
		res.Resources = append(res.Resources, rs...)
	}
	rs, err := Docker()
	add(SrcDocker, rs, err)
	rs, err = Simulators(cfg)
	add(SrcSimctl, rs, err)
	if procs == nil {
		res.Failed[SrcProcs] = fmt.Errorf("could not read the process table")
	} else {
		add(SrcProcs, Processes(cfg, procs), nil)
	}
	sort.Slice(res.Resources, func(i, j int) bool { return res.Resources[i].ID < res.Resources[j].ID })
	return res
}

// ---- Docker ----

var hostPortRe = regexp.MustCompile(`:(\d+)->`)
var memUsageRe = regexp.MustCompile(`^([\d.]+)\s*([KMG]i?B|kB|B)`)

func parseSize(s string) float64 { // in MB
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
	case "KiB", "KB", "kB":
		return v / 1024
	}
	return v / 1048576
}

// Docker groups running containers: one resource per kind cluster, per Compose project (and
// Supabase stack), or per standalone container.
func Docker() ([]*model.Resource, error) {
	if off, until := sys.BackedOff("docker"); off {
		return nil, fmt.Errorf("docker has been timing out; skipped until %s", until.Format("15:04:05"))
	}
	out, err := sys.Run(5*time.Second, "docker", "ps", "--format",
		`{{.Names}}	{{.Label "io.x-k8s.kind.cluster"}}	{{.Label "com.docker.compose.project"}}	{{.Label "com.supabase.cli.project"}}	{{.Image}}	{{.Ports}}	{{.Label "com.docker.compose.project.working_dir"}}`)
	if err != nil {
		if strings.Contains(err.Error(), "Cannot connect to the Docker daemon") || strings.Contains(err.Error(), "executable file not found") {
			return nil, nil // Docker is not running or not installed: nothing of its is running either
		}
		return nil, err
	}
	type stat struct{ mem, cpu, net float64 }
	stats := map[string]stat{}
	if s, err := sys.Run(10*time.Second, "docker", "stats", "--no-stream", "--format", "{{.Name}}\t{{.MemUsage}}\t{{.CPUPerc}}\t{{.NetIO}}"); err == nil {
		for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
			f := strings.Split(l, "\t")
			if len(f) < 4 {
				continue
			}
			cpu, _ := strconv.ParseFloat(strings.TrimSuffix(f[2], "%"), 64)
			net := 0.0
			for _, side := range strings.Split(f[3], "/") {
				net += parseSize(side)
			}
			stats[f[0]] = stat{parseSize(f[1]), cpu, net * 1048576}
		}
	}
	groups := map[string]*model.Resource{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(l, "\t")
		if len(f) < 5 || f[0] == "" {
			continue
		}
		get := func(i int) string {
			if i < len(f) {
				return f[i]
			}
			return ""
		}
		name, kindCluster, compose, supa, image, ports, wd := f[0], f[1], f[2], f[3], f[4], get(5), get(6)
		var id, kind, gname string
		switch {
		case kindCluster != "":
			id, kind, gname = "kind:"+kindCluster, model.RKCluster, kindCluster
		case supa != "":
			id, kind, gname = "compose:supabase-"+supa, model.RKContainers, "supabase "+supa
		case compose != "":
			id, kind, gname = "compose:"+compose, model.RKContainers, compose
		default:
			id, kind, gname = "docker:"+name, model.RKContainers, name+" ("+image+")"
		}
		g := groups[id]
		if g == nil {
			g = &model.Resource{ID: id, Kind: kind, Name: gname, Cwd: wd}
			groups[id] = g
		}
		g.Containers = append(g.Containers, name)
		st := stats[name]
		g.MemMB += st.mem
		g.CPU += st.cpu
		g.NetBytes += st.net
		for _, hp := range hostPortRe.FindAllStringSubmatch(ports, -1) {
			if !contains(g.Ports, hp[1]) {
				g.Ports = append(g.Ports, hp[1])
			}
		}
	}
	var res []*model.Resource
	for _, g := range groups {
		g.Detail = fmt.Sprintf("%d container(s)", len(g.Containers))
		g.Restart = "docker start " + strings.Join(g.Containers, " ")
		res = append(res, g)
	}
	return res, nil
}

// ---- Simulators ----

func Simulators(cfg model.Config) ([]*model.Resource, error) {
	out, err := sys.Run(5*time.Second, "xcrun", "simctl", "list", "devices", "booted", "-j")
	if err != nil {
		if strings.Contains(err.Error(), "executable file not found") || strings.Contains(err.Error(), "unable to find utility") {
			return nil, nil // no Xcode
		}
		return nil, err
	}
	var parsed struct {
		Devices map[string][]struct {
			UDID  string `json:"udid"`
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"devices"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		return nil, err
	}
	var res []*model.Resource
	for runtime, devs := range parsed.Devices {
		for _, d := range devs {
			if d.State != "Booted" {
				continue
			}
			rt := runtime[strings.LastIndex(runtime, ".")+1:]
			res = append(res, &model.Resource{ID: "sim:" + d.UDID, Kind: model.RKSimulator, Name: d.Name,
				Detail: rt, UDID: d.UDID, MemMB: cfg.KindMemGB[model.KSimulator] * 1024,
				Restart: "xcrun simctl boot " + d.UDID})
		}
	}
	return res, nil
}

// ---- Processes ----

var avdRe = regexp.MustCompile(`-avd\s+(\S+)`)
var xmxRe = regexp.MustCompile(`-Xmx(\S+)`)
var limaRe = regexp.MustCompile(`limactl\S*\s+hostagent\b.*\s(\S+)$`)
var tartRe = regexp.MustCompile(`\btart\s+run\s+(\S+)`)

var databases = map[string]string{ // process name -> brew formula prefix
	"postgres": "postgresql", "mysqld": "mysql", "redis-server": "redis", "mongod": "mongodb",
	"mariadbd": "mariadb", "valkey-server": "valkey", "memcached": "memcached",
}

// Processes finds process-backed resources: Gradle and Kotlin daemons, dev servers, Android
// emulators, VMs, local model servers, Homebrew databases and headless browsers.
func Processes(cfg model.Config, procs map[int]*sys.Proc) []*model.Resource {
	var res []*model.Resource
	sub := subtreeMB(procs)
	for _, p := range procs {
		base := filepath.Base(p.Exe)
		a := p.Args
		switch {
		case strings.Contains(a, "org.gradle.launcher.daemon") || strings.Contains(a, "GradleDaemon"):
			res = append(res, procRes(p, "gradle", model.RKGradle, "Gradle daemon", heap(a), p.RSSMB))
		case strings.Contains(a, "KotlinCompileDaemon"):
			res = append(res, procRes(p, "gradle", model.RKGradle, "Kotlin daemon", heap(a), p.RSSMB))
		case strings.HasPrefix(base, "qemu-system") && avdRe.MatchString(a):
			name := avdRe.FindStringSubmatch(a)[1]
			res = append(res, procRes(p, "emu", model.RKEmulator, "Android emulator "+name, "AVD "+name, sub[p.PID]))
		case strings.HasPrefix(base, "qemu-system") && !underApp(procs, p, "Docker.app"):
			res = append(res, procRes(p, "vm", model.RKVM, "QEMU VM", trunc(a, 90), sub[p.PID]))
		case limaRe.MatchString(a):
			name := limaRe.FindStringSubmatch(a)[1]
			r := procRes(p, "vm", model.RKVM, "Lima VM "+name, "limactl", sub[p.PID])
			r.ID, r.Restart = "vm:lima-"+name, "limactl start "+name
			res = append(res, r)
		case tartRe.MatchString(a):
			name := tartRe.FindStringSubmatch(a)[1]
			r := procRes(p, "vm", model.RKVM, "Tart VM "+name, "tart", sub[p.PID])
			r.ID, r.Restart = "vm:tart-"+name, "tart run "+name
			res = append(res, r)
		case base == "ollama" && strings.Contains(a, " serve"):
			if mb := sub[p.PID] + p.RSSMB; mb > 300 { // only when models are loaded
				r := procRes(p, "model", model.RKModel, "Ollama", "loaded models", mb)
				r.ID, r.Restart = "model:ollama", "models load again on the next request"
				res = append(res, r)
			}
		case base == "llama-server" || strings.Contains(a, "mlx_lm.server") || strings.Contains(a, "vllm serve"):
			res = append(res, procRes(p, "model", model.RKModel, base, trunc(a, 90), p.RSSMB+sub[p.PID]))
		case databases[base] != "" && isRoot(procs, p, base) &&
			(strings.Contains(p.Exe, "/homebrew/") || strings.Contains(p.Exe, "/usr/local/")):
			r := procRes(p, "db", model.RKDatabase, base, "Homebrew service", p.RSSMB+sub[p.PID])
			r.ID = "db:" + base
			r.Restart = "brew services start " + formula(p.Exe, databases[base])
			res = append(res, r)
		case isHeadlessBrowser(a) && !isHeadlessBrowserParent(procs, p):
			res = append(res, procRes(p, "browser", model.RKBrowser, "headless "+browserName(a), "", p.RSSMB+sub[p.PID]))
		}
	}
	// Listening dev servers.
	for pid, ps := range sys.ListeningPorts() {
		p := procs[pid]
		if p == nil || !IsDevServer(p, cfg) {
			continue
		}
		r := procRes(p, "srv", model.RKServer, filepath.Base(p.Exe)+" :"+strings.Join(ps, ","), trunc(p.Args, 90), p.RSSMB)
		r.Ports = ps
		res = append(res, r)
	}
	// Where dev servers and model servers run, for per-project estimates.
	var pids []int
	for _, r := range res {
		if r.Kind == model.RKServer || r.Kind == model.RKModel {
			pids = append(pids, r.PID)
		}
	}
	cwds := sys.Cwds(pids)
	for _, r := range res {
		if c := cwds[r.PID]; c != "" && r.Cwd == "" {
			r.Cwd = c
		}
	}
	return res
}

func procRes(p *sys.Proc, prefix, kind, name, detail string, mb float64) *model.Resource {
	return &model.Resource{ID: fmt.Sprintf("%s:%d", prefix, p.PID), Kind: kind, Name: name, Detail: detail,
		PID: p.PID, MemMB: mb, CPU: p.CPU, Restart: "re-run the command that started it"}
}

// subtreeMB is the memory of each process's descendants (not including itself).
func subtreeMB(procs map[int]*sys.Proc) map[int]float64 {
	out := map[int]float64{}
	for _, p := range procs {
		seen := 0
		for q := procs[p.PPID]; q != nil && q.PID > 1 && seen < 64; q = procs[q.PPID] {
			out[q.PID] += p.RSSMB
			seen++
		}
	}
	return out
}

func underApp(procs map[int]*sys.Proc, p *sys.Proc, app string) bool {
	for i, q := 0, p; q != nil && q.PID > 1 && i < 64; i, q = i+1, procs[q.PPID] {
		if strings.Contains(q.Exe, "/"+app+"/") {
			return true
		}
	}
	return false
}

// isRoot reports whether p is the top process of its kind (postgres forks many postgres).
func isRoot(procs map[int]*sys.Proc, p *sys.Proc, base string) bool {
	par := procs[p.PPID]
	return par == nil || filepath.Base(par.Exe) != base
}

func formula(exe, prefix string) string {
	// /opt/homebrew/opt/postgresql@16/bin/postgres → postgresql@16
	for _, part := range strings.Split(exe, "/") {
		if strings.HasPrefix(part, prefix) {
			return part
		}
	}
	return prefix
}

var headlessRe = regexp.MustCompile(`(?i)(chrome-headless-shell|--headless|ms-playwright|puppeteer)`)

func isHeadlessBrowser(args string) bool {
	return headlessRe.MatchString(args) && regexp.MustCompile(`(?i)(chrom|firefox|webkit)`).MatchString(args) &&
		!strings.Contains(args, "--type=") // renderer, GPU and utility helpers are part of the parent
}

func isHeadlessBrowserParent(procs map[int]*sys.Proc, p *sys.Proc) bool {
	par := procs[p.PPID]
	return par != nil && isHeadlessBrowser(par.Args)
}

func browserName(args string) string {
	low := strings.ToLower(args)
	switch {
	case strings.Contains(low, "firefox"):
		return "Firefox"
	case strings.Contains(low, "webkit"):
		return "WebKit"
	}
	return "Chromium"
}

// IsDevServer decides whether a listening process is a dev server AgentRM manages.
func IsDevServer(p *sys.Proc, cfg model.Config) bool {
	for _, pre := range []string{"/Applications/", "/System/", "/Library/", "/usr/libexec/", "/usr/sbin/", "/sbin/"} {
		if strings.HasPrefix(p.Exe, pre) {
			return false
		}
	}
	low := strings.ToLower(p.Args)
	for _, x := range cfg.ServerExclude {
		if strings.Contains(low, strings.ToLower(x)) {
			return false
		}
	}
	base := filepath.Base(p.Exe)
	if databases[base] != "" || base == "ollama" || base == "llama-server" {
		return false // found as their own kinds
	}
	if strings.Contains(p.Exe, "/go-build") {
		return true
	}
	for _, n := range cfg.ServerNames {
		if base == n || (strings.HasSuffix(n, "*") && strings.HasPrefix(base, strings.TrimSuffix(n, "*"))) {
			return true
		}
	}
	return false
}

func heap(args string) string {
	if mm := xmxRe.FindStringSubmatch(args); mm != nil {
		return "max heap " + mm[1]
	}
	return ""
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
