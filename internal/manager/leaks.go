package manager

// Leak watch. A dev process whose memory footprint keeps climbing for an hour, with nothing to
// explain it, is flagged and you are asked whether to restart it: a stuck tsserver or language
// server, a dev server that leaks, a long-lived Python process. Nothing is ever done to it
// automatically; accepting is yours, in the app or with agentrm leak accept.
//
// Cheap by construction: only processes ps already shows at a quarter of the tracking size are
// measured, only those at leakTrackMB or more are tracked, and each keeps a ring of at most
// leakRing samples taken no closer than leakSampleEvery. A track is keyed by PID and start
// time, so a reused PID starts afresh.

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/discover"
	"agentrm/internal/model"
	"agentrm/internal/sys"
)

// What counts as a leak. Chosen so that a garbage-collected runtime's sawtooth, a cache warming
// up, or a one-off jump is not one; only a sustained, mostly one-way climb is.
const (
	// leakTrackMB: only processes with at least this footprint are tracked. Smaller ones cannot
	// grow by leakMinGrowthMB without passing it first, and are not worth a question.
	leakTrackMB = 500
	// leakSampleEvery: at most one sample per process per interval. A leak is a trend over an
	// hour; sampling every sweep (30 s) would only add noise and cost.
	leakSampleEvery = 5 * time.Minute
	// leakRing: samples kept per process, about two hours at leakSampleEvery.
	leakRing = 24
	// leakMinSamples and leakMinSpan: a verdict needs at least an hour of history, with at
	// least 12 points in it, so that one or two odd readings cannot carry it.
	leakMinSamples = 12
	leakMinSpan    = 60 * time.Minute
	// leakMinGrowthMB and leakMinGrowthFrac: it must have grown by max(500 MB, 50% of where it
	// started). The absolute floor ignores small processes doubling; the share ignores big ones
	// that move by a few hundred MB as a matter of course.
	leakMinGrowthMB   = 500
	leakMinGrowthFrac = 0.5
	// leakFitShare: the least-squares line over the window must account for at least this
	// share of that growth, so that one spike at the end of a flat run is not a climb.
	leakFitShare = 0.75
	// leakMonotone: at least this share of steps must be non-negative, where a step down of
	// less than leakStepTolerance of the footprint counts as flat (allocator and GC jitter). A
	// sawtooth fails it: half its steps go down.
	leakMonotone      = 0.8
	leakStepTolerance = 0.02
	// leakRecentShare: the second half of the window must hold at least this share of the
	// growth. A process that climbed once and has been flat since is not climbing now.
	leakRecentShare = 0.25
	// leakApprovalTTL: how long a question about a leak waits for an answer before it is asked
	// again. Longer than a privileged request's: nobody is waiting at a terminal for it.
	leakApprovalTTL = 12 * time.Hour
)

type leakSample struct {
	at time.Time
	mb float64
}

// leakTrack is one process being watched.
type leakTrack struct {
	pid     int
	start   time.Time
	name    string
	cmd     string
	samples []leakSample
	leak    *model.Leak // set while it is flagged
	decided bool        // the dry-run decision for this leak is recorded
	held    bool        // the fail-open hold on asking is reported
}

// Expected growth: what these run as is meant to take memory as it works (the Docker VM, an
// emulator or simulator, a browser, Xcode, a model server, a database's cache), or a build is
// running. Matched at the start of a word in the executable's name or its command line, so
// "tart" does not match "npm start".
var (
	leakExpected = regexp.MustCompile(`(^|[^a-z0-9])(docker|com\.docker|qemu|vfkit|colima|lima|virtualiz|orbstack|podman|krunkit|tart|utm|` +
		`emulator|netsimd|simulator|coresimulator|launchd_sim|` +
		`chrome|chromium|firefox|safari|webkit|brave|msedge|headless_shell|opera|vivaldi|` +
		`xcode|sourcekit|xcbbuildservice|ibagent|` +
		`ollama|llama|lmstudio|lm studio|mlx|vllm|koboldcpp|localai|text-generation|` +
		`postgres|mysqld|mariadbd|mongod|redis-server|clickhouse|elasticsearch|opensearch)`)
	leakBuild = regexp.MustCompile(`(^|[^a-z0-9])(cargo|rustc|clang|swift-frontend|swiftc|javac|kotlinc|xcodebuild|gradle|` +
		`kotlincompiledaemon|bazel|buck2?|esbuild|cc1|cc1plus)([^a-z0-9]|$)|\sbuild(\s|$)`)
	// leakDevHelper: a language server an editor runs from inside its app bundle is still a dev
	// process (VS Code starts tsserver again when it exits).
	leakDevHelper = regexp.MustCompile(`tsserver|typescript-language-server|language-?server|langserver|[a-z]-lsp([^a-z]|$)|` +
		`gopls|rust-analyzer|pyright|pylance|eslintserver|intelephense`)
)

// Resource kinds whose memory growth is expected.
var leakExpectedKinds = map[string]bool{model.RKVM: true, model.RKEmulator: true, model.RKSimulator: true,
	model.RKModel: true, model.RKBrowser: true, model.RKCluster: true, model.RKContainers: true,
	model.RKDatabase: true, model.RKGradle: true, model.RKStaleAgent: true,
	// These two have their own reclaim (dockervm.go, xcode helpers); asking twice helps nobody.
	model.RKDockerVM: true, model.RKXcodeHelper: true}

// leakText is what a process says it is, lower-cased: its executable's name and its command
// line (bounded).
func leakText(p *sys.Proc) string {
	exe := p.Path
	if exe == "" {
		exe = p.Exe
	}
	return strings.ToLower(filepath.Base(exe) + " " + trunc(p.Args, 300))
}

// leakNeverWatched says why a process is never a leak candidate, whatever it does, or "". These
// do not change over a process's life, so they are checked once, before it is tracked.
func (m *Manager) leakNeverWatched(p *sys.Proc) string {
	exe := p.Path
	if exe == "" {
		exe = p.Exe
	}
	text := leakText(p)
	switch {
	case p.PID <= 1 || p.PID == m.selfPID:
		return "AgentRM itself"
	case p.Start.IsZero():
		return "its start time is unknown, so a reused PID could not be told apart"
	case sys.AgentKind(p) != "":
		return "an agent"
	case systemBinary(exe):
		return "a system process"
	case isApp(exe) && !leakDevHelper.MatchString(text):
		return "an app"
	case leakExpected.MatchString(text):
		return "its growth is expected"
	}
	if m.procUID != nil {
		if uid, ok := m.procUID(p.PID); ok && uid == 0 {
			return "a system process (root)"
		}
	}
	return ""
}

// watchLeaks samples large processes and flags those whose footprint keeps climbing with
// nothing to explain it. Must hold mu.
func (m *Manager) watchLeaks(procs map[int]*sys.Proc, now time.Time) {
	if m.leakTracks == nil {
		m.leakTracks = map[procKey]*leakTrack{}
	}
	m.leakProcs = procs
	live := map[procKey]bool{}
	for pid, p := range procs {
		key := procKey{pid, p.Start.Unix()}
		t := m.leakTracks[key]
		if t == nil {
			// ps's RSS leaves compressed pages out, so the pre-filter is a quarter of the size.
			if p.RSSMB < leakTrackMB/4 || m.leakNeverWatched(p) != "" {
				continue
			}
			mb := m.procMB(p)
			if mb < leakTrackMB {
				continue
			}
			t = &leakTrack{pid: pid, start: p.Start, name: filepath.Base(p.Exe), cmd: trunc(p.Args, 120)}
			m.leakTracks[key] = t
		}
		live[key] = true
		if n := len(t.samples); n == 0 || now.Sub(t.samples[n-1].at) >= leakSampleEvery {
			t.samples = append(t.samples, leakSample{at: now, mb: m.procMB(p)})
			if len(t.samples) > leakRing {
				t.samples = append(t.samples[:0], t.samples[len(t.samples)-leakRing:]...)
			}
		}
	}
	// Dead processes, and PIDs now someone else's: their history goes with them.
	for key, t := range m.leakTracks {
		if !live[key] {
			m.unflag(t, "it exited")
			delete(m.leakTracks, key)
		}
	}
	for _, t := range m.leakTracks {
		m.judgeLeak(t, procs, now)
	}
}

// judgeLeak flags, updates or clears one track. Must hold mu.
func (m *Manager) judgeLeak(t *leakTrack, procs map[int]*sys.Proc, now time.Time) {
	res, why := m.leakExplained(t.pid, procs)
	if why != "" {
		m.unflag(t, why)
		return
	}
	ok, rate := climbing(t.samples)
	if t.leak != nil && !ok && !t.leak.Dismissed {
		m.unflag(t, "it stopped climbing")
		return
	}
	if t.leak == nil {
		if !ok {
			return
		}
		first, last := t.samples[0], t.samples[len(t.samples)-1]
		action := "stop"
		if res != nil && discover.Restartable(res) && len(res.RestartArgv) > 0 {
			action = "restart"
		}
		l := &model.Leak{ID: "leak-" + newID(), PID: t.pid, ProcStart: t.start, Name: t.name, Cmd: t.cmd,
			Since: first.at, StartMB: first.mb, NowMB: last.mb, RateMBh: rate, Flagged: now, Action: action}
		if res != nil && res.Kind != model.RKProcess {
			l.Resource = res.ID
		}
		t.leak, t.decided, t.held = l, false, false
		m.event("LEAK %s (pid %d) grew from %s to %s in %s (%+.0f MB/h) with nothing to explain it. "+
			"Accept to %s it: agentrm leak accept %s, or leave it: agentrm leak dismiss %s",
			l.Name, l.PID, gb(l.StartMB), gb(l.NowMB), last.at.Sub(first.at).Round(time.Minute), rate, action, l.ID, l.ID)
		m.notify("Leaking: "+l.Name, fmt.Sprintf("Grew from %s to %s in %s. %s it?", gb(l.StartMB), gb(l.NowMB),
			last.at.Sub(first.at).Round(time.Minute), strings.ToUpper(action[:1])+action[1:]))
	} else if n := len(t.samples); n > 0 {
		t.leak.NowMB = t.samples[n-1].mb
		if rate > 0 {
			t.leak.RateMBh = rate
		}
	}
	m.askLeak(t, res, now)
}

// askLeak puts a flagged leak to you as an approval, unless AgentRM is letting everything
// through, it is in dry run for this resource, or you already answered. Must hold mu.
func (m *Manager) askLeak(t *leakTrack, res *model.Resource, now time.Time) {
	l := t.leak
	if l.Dismissed {
		return
	}
	if a := m.st.Approvals[l.Approval]; a != nil {
		if a.State == "pending" && now.After(a.Expires) {
			a.State = "expired"
		}
		switch a.State {
		case "pending", "approved", "failed":
			return
		case "denied":
			l.Dismissed = true
			m.event("LEAK %s (pid %d) is left alone: you said no", l.Name, l.PID)
			return
		}
		// Expired: asked again below.
	}
	l.Approval = ""
	if why := m.failOpen(now); why != "" {
		if !t.held {
			t.held = true
			m.event("HELD the question about leaking %s (pid %d): %s", l.Name, l.PID, why)
		}
		return
	}
	if m.dryFor(res) {
		if !t.decided {
			t.decided = true
			m.decide(l.Action, res, leakWhy(l))
		}
		return
	}
	b, _ := json.Marshal(api.Req{ResourceID: l.ID, Approve: ptr(true), Via: "app"})
	var req map[string]any
	_ = json.Unmarshal(b, &req)
	a := &model.Approval{ID: newID(), What: leakWhat(l), Created: now, Expires: now.Add(leakApprovalTTL),
		Request: req, Path: "/leak", From: "AgentRM", State: "pending"}
	m.st.Approvals[a.ID] = a
	l.Approval = a.ID
	m.event("APPROVAL %s: %s. Approve or deny it in AgentRM.", a.ID, a.What)
}

func ptr[T any](v T) *T { return &v }

func leakWhy(l *model.Leak) string {
	return fmt.Sprintf("its memory grew from %s to %s since %s (%+.0f MB/h) with nothing to explain it",
		gb(l.StartMB), gb(l.NowMB), l.Since.Format("15:04"), l.RateMBh)
}

func leakWhat(l *model.Leak) string {
	what := l.Name
	if l.Resource != "" {
		what = l.Resource
	}
	return fmt.Sprintf("%s leaking %s (pid %d)? %s",
		strings.ToUpper(l.Action[:1])+l.Action[1:], what, l.PID, strings.ToUpper(leakWhy(l)[:1])+leakWhy(l)[1:])
}

// unflag clears a flagged leak; a question still waiting about it is withdrawn. Must hold mu.
func (m *Manager) unflag(t *leakTrack, why string) {
	if t.leak == nil {
		return
	}
	if a := m.st.Approvals[t.leak.Approval]; a != nil && a.State == "pending" {
		a.State = "expired"
	}
	if !t.leak.Dismissed {
		m.event("LEAK %s (pid %d) is no longer flagged: %s", t.leak.Name, t.leak.PID, why)
	}
	t.leak = nil
}

// leakExplained finds what the process is part of (the nearest tracked resource at or above it,
// else the process itself as a bare resource), and says why its growth is not a leak, or "".
// Must hold mu.
func (m *Manager) leakExplained(pid int, procs map[int]*sys.Proc) (*model.Resource, string) {
	p := procs[pid]
	if p == nil {
		return nil, "it exited"
	}
	if leakBuild.MatchString(leakText(p)) {
		return nil, "a build is running"
	}
	byPID := map[int]*model.Resource{}
	for _, r := range m.st.Resources {
		if r.PID > 0 {
			byPID[r.PID] = r
		}
	}
	var res *model.Resource
	for q, i := p, 0; q != nil && q.PID > 1 && i < 64; q, i = procs[q.PPID], i+1 {
		if r := byPID[q.PID]; r != nil && sys.SameProcess(q, r.ProcStart) {
			res = r
			break
		}
		if q != p && leakBuild.MatchString(leakText(q)) {
			return nil, "a build is running"
		}
	}
	if res == nil {
		res = &model.Resource{ID: "proc:" + strconv.Itoa(pid), Kind: model.RKProcess, Source: "procs",
			Name: filepath.Base(p.Exe), Detail: trunc(p.Args, 90), PID: pid, ProcStart: p.Start, MemMB: m.procMB(p),
			Restart: "re-run the command that started it"}
	}
	switch {
	case leakExpectedKinds[res.Kind]:
		return res, "its growth is expected (" + res.Kind + ")"
	case res.LeaseID != "":
		return res, "lease " + res.LeaseID + " holds it"
	case res.Pinned:
		return res, "it is pinned"
	case res.AskOnly:
		return res, "AgentRM never stops it"
	case m.neverStop(res) || m.neverStopCmd(p.Args):
		return res, "it is exempt in your settings (never_stop)"
	}
	// An agent's tree, while that session holds a lease: its growth is that lease's work.
	if apid, kind := sys.AgentAncestor(procs, pid); apid > 0 {
		s := m.sessionOf(apid, kind)
		for _, l := range m.st.Leases {
			if l.Session == s {
				return res, "session " + short(s) + " holds lease " + l.ID
			}
		}
	}
	return res, ""
}

// neverStopCmd says whether a never_stop entry names a process's command line.
func (m *Manager) neverStopCmd(args string) bool {
	for _, x := range m.cfg.NeverStop {
		if x != "" && strings.Contains(args, x) {
			return true
		}
	}
	return false
}

// climbing says whether samples show a sustained climb, and the fitted rate in MB per hour.
func climbing(s []leakSample) (bool, float64) {
	n := len(s)
	if n < 2 {
		return false, 0
	}
	span := s[n-1].at.Sub(s[0].at)
	// Least-squares slope of footprint against time, in MB per hour.
	var mx, my float64
	for _, x := range s {
		mx += x.at.Sub(s[0].at).Hours()
		my += x.mb
	}
	mx, my = mx/float64(n), my/float64(n)
	var sxy, sxx float64
	for _, x := range s {
		dx := x.at.Sub(s[0].at).Hours() - mx
		sxy += dx * (x.mb - my)
		sxx += dx * dx
	}
	rate := 0.0
	if sxx > 0 {
		rate = sxy / sxx
	}
	if n < leakMinSamples || span < leakMinSpan || rate <= 0 {
		return false, rate
	}
	first, last := s[0].mb, s[n-1].mb
	need := max(leakMinGrowthMB, leakMinGrowthFrac*first)
	if last-first < need || rate*span.Hours() < leakFitShare*need {
		return false, rate
	}
	if last-s[n/2].mb < leakRecentShare*(last-first) {
		return false, rate // climbed, and has been flat since
	}
	up := 0
	for i := 1; i < n; i++ {
		if s[i].mb-s[i-1].mb >= -leakStepTolerance*s[i-1].mb {
			up++
		}
	}
	return float64(up) >= leakMonotone*float64(n-1), rate
}

// leakList is what is flagged now, for status, oldest first.
func (m *Manager) leakList() []*model.Leak {
	var out []*model.Leak
	for _, t := range m.leakTracks {
		if t.leak != nil {
			out = append(out, t.leak)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Flagged.Before(out[j].Flagged) })
	return out
}

// findLeak finds a flagged leak by its ID, or by PID when exactly one flagged leak has it.
func (m *Manager) findLeak(id string) *leakTrack {
	var byPID []*leakTrack
	for _, t := range m.leakTracks {
		if t.leak == nil {
			continue
		}
		if t.leak.ID == id {
			return t
		}
		if strconv.Itoa(t.pid) == id {
			byPID = append(byPID, t)
		}
	}
	if len(byPID) == 1 {
		return byPID[0]
	}
	return nil
}

// leakAct answers a flagged leak: accept (restart it if it has a known restart, else stop it
// with SIGTERM) or dismiss (leave it, and do not ask again). Only a person may; from a terminal
// it waits for a click in the app (privileged). Run by an approval from the app too.
func (m *Manager) leakAct(c api.Caller, r api.Req) (any, []Effect, error) {
	dismiss := r.Approve != nil && !*r.Approve
	verb := "act on"
	if dismiss {
		verb = "dismiss"
	}
	if !c.Human {
		return nil, nil, fmt.Errorf("only a person can %s a leak; an agent cannot", verb)
	}
	t := m.findLeak(r.ResourceID)
	if t == nil {
		return nil, nil, fmt.Errorf("no leaking process %q is flagged now (see agentrm status)", r.ResourceID)
	}
	l := t.leak
	if !dismiss {
		verb = l.Action
	}
	if err := m.privileged(c, fmt.Sprintf("%s leaking %s (pid %d)", verb, l.Name, l.PID)); err != nil {
		return nil, nil, err
	}
	answer := func(state string) {
		if a := m.st.Approvals[l.Approval]; a != nil && a.State == "pending" {
			a.State = state
		}
	}
	if dismiss {
		answer("denied")
		l.Dismissed = true
		m.event("LEAK %s (pid %d) is left alone: dismissed by %s", l.Name, l.PID, c.Session)
		return &api.CountResp{N: 1, Note: "left alone; AgentRM will not ask about it again"}, nil, nil
	}
	// Against the table of the last sweep, which watched it: a process gone since is caught by
	// the start-time check made right before any signal is sent.
	procs := m.leakProcs
	if p := procs[t.pid]; p == nil || !sys.SameProcess(p, t.start) {
		return nil, nil, fmt.Errorf("%s (pid %d) is gone, or its PID is now another process; nothing was done", l.Name, l.PID)
	}
	res, why := m.leakExplained(t.pid, procs)
	switch {
	case why != "":
		m.unflag(t, why)
		return nil, nil, fmt.Errorf("%s (pid %d) is no longer flagged: %s", l.Name, l.PID, why)
	case m.dryFor(res):
		return nil, nil, fmt.Errorf("dry run is on, so AgentRM only reports leaks and acts on none; "+
			"stop %s (pid %d) yourself, or turn dry run off: agentrm config --dry-run=false", l.Name, l.PID)
	case m.pending[res.ID] != "":
		return nil, nil, fmt.Errorf("%s is busy (%s in progress); try again in a moment", res.ID, m.pending[res.ID])
	}
	op := l.Action
	if op == "restart" && (!discover.Restartable(res) || len(res.RestartArgv) == 0) {
		op = "stop"
	}
	why = leakWhy(l) + "; " + map[string]string{"restart": "restarted", "stop": "stopped"}[op] + " at your word"
	answer("approved")
	m.decide(op, res, why)
	m.pending[res.ID] = op
	t.leak = nil // asked again only if it climbs again (a failed stop, say)
	t.samples = nil
	note := "restarting it"
	if op == "stop" {
		note = "stopping it with SIGTERM"
		if res.Kind != model.RKProcess {
			note += "; bring it back with agentrm restore " + res.ID
		}
	}
	return &api.CountResp{N: 1, Note: note}, []Effect{{Op: op, Res: *res, Why: why, By: c.Session}}, nil
}
