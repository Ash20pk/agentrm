package manager

// Replay runs a policy over a recorded trace, offline, on the trace's own clock, and scores it
// against the trace itself: whatever the policy pauses or stops, the recording shows whether it
// was used again afterwards (a connection, traffic, CPU, a command naming it). That makes a
// policy change measurable before it touches a real machine.
//
// Two policies can be replayed:
//
//   - "agentrm": the manager with a given config (auto-stop on, so every decision is taken);
//   - "reaper": the rule the cleanup scripts use — kill anything re-parented to launchd that
//     looks like a dev process — for comparison on the same trace.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"

	"agentrm/internal/discover"
	"agentrm/internal/evidence"
	"agentrm/internal/model"
	"agentrm/internal/sys"
	"agentrm/internal/trace"
)

type ReplayOptions struct {
	Policy string        // "agentrm" (default) or "reaper"
	Config *model.Config // for "agentrm"; nil: defaults scaled to the recorded machine
	// ReuseWindow: a stop counts as wrong if the thing is used again within this long.
	ReuseWindow time.Duration
}

// Stop is one pause or stop a replayed policy made, and what the recording says came after.
type ReplayStop struct {
	Op        string    `json:"op"` // pause, stop, kill
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	MemMB     float64   `json:"mem_mb"`
	At        time.Time `json:"at"`
	UsedAt    time.Time `json:"used_at,omitempty"` // first use after it, if any (within the window)
	Why       string    `json:"why"`
	LiveOwner bool      `json:"live_owner,omitempty"` // its agent was still running when it was taken
}

func (s ReplayStop) Wrong() bool { return !s.UsedAt.IsZero() }

type ReplayReport struct {
	Policy string        `json:"policy"`
	From   time.Time     `json:"from"`
	To     time.Time     `json:"to"`
	Frames int           `json:"frames"`
	Stops  []*ReplayStop `json:"stops"`
	Pauses int           `json:"pauses"`
	// PausesUsed: paused, then used while paused (AgentRM thaws it; a cost, not a loss).
	PausesUsed int `json:"pauses_used"`
	StopsN     int `json:"stops_n"`
	WrongStops int `json:"wrong_stops"`
	// LiveOwnerStops: taken while the agent that started it was still running.
	LiveOwnerStops int `json:"live_owner_stops"`
	// FreedGBh is memory freed by stops, in GB-hours, until the end or the next use.
	FreedGBh float64 `json:"freed_gb_hours"`
	// Queue: requests that had to wait, and for how long until granted.
	Queued   int           `json:"queued"`
	Granted  int           `json:"granted"`
	WaitMean time.Duration `json:"wait_mean"`
	WaitMax  time.Duration `json:"wait_max"`
	// Agreement with the live daemon, for a trace recorded with the same config: the share of
	// its recorded decisions the replay also took (same action, same resource, within a sweep).
	LiveDecisions int     `json:"live_decisions"`
	Agreement     float64 `json:"agreement"`
	// The command-stream predictor on this trace, against the most-frequent-kind baseline.
	Predicted, PredictHits, BaselineHits int
}

// usage is the ground truth: when each resource and process was seen in use, from the recording.
type usage struct {
	byID  map[string][]time.Time
	byPID map[procKey][]time.Time
}

func (u *usage) after(ts []time.Time, t time.Time, window time.Duration) time.Time {
	i := sort.Search(len(ts), func(i int) bool { return ts[i].After(t) })
	if i < len(ts) && (window <= 0 || ts[i].Sub(t) <= window) {
		return ts[i]
	}
	return time.Time{}
}

// groundTruth reads a trace once and notes every sign of use, whatever any policy would do.
func groundTruth(path string) (*usage, error) {
	rd, err := trace.Open(path)
	if err != nil {
		return nil, err
	}
	u := &usage{byID: map[string][]time.Time{}, byPID: map[procKey][]time.Time{}}
	act := evidence.NewActivity()
	last := map[string]*model.Resource{}
	for {
		f, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch {
		case f.Sweep != nil:
			for _, r := range f.Sweep.Found {
				last[r.ID] = r
				if act.Active(r, f.Sweep.Est) {
					u.byID[r.ID] = append(u.byID[r.ID], f.T)
				}
			}
			for _, p := range rd.Table() {
				if p.CPU >= 3 {
					k := keyOf(p)
					u.byPID[k] = append(u.byPID[k], f.T)
				}
			}
		case f.Request != nil && f.Request.Path == "/touch" && f.Request.Req.Cmd != "":
			for _, id := range evidence.TouchedBy(f.Request.Req.Cmd, f.Request.Req.Cwd, last, "") {
				u.byID[id] = append(u.byID[id], f.T)
			}
		case f.Request != nil && f.Request.Path == "/claim" && f.Request.Req.ResourceID != "":
			u.byID[f.Request.Req.ResourceID] = append(u.byID[f.Request.Req.ResourceID], f.T)
		}
	}
	return u, nil
}

// Replay runs one policy over a trace (a file, or a directory of daily files).
func Replay(path string, o ReplayOptions) (*ReplayReport, error) {
	if o.ReuseWindow == 0 {
		o.ReuseWindow = 24 * time.Hour
	}
	truth, err := groundTruth(path)
	if err != nil {
		return nil, err
	}
	if o.Policy == "reaper" {
		return replayReaper(path, truth, o)
	}
	return replayAgentRM(path, truth, o)
}

// debugReplay, when set (tests), logs the replay's decisions.
var debugReplay func(format string, a ...any)

// replayClock is the trace's clock.
type replayClock struct{ t time.Time }

func (c *replayClock) Now() time.Time { return c.t }

func replayAgentRM(path string, truth *usage, o ReplayOptions) (*ReplayReport, error) {
	rd, err := trace.Open(path)
	if err != nil {
		return nil, err
	}
	clk := &replayClock{}
	m := New(Options{NoDisk: true, Now: clk.Now})
	m.notify = func(string, string) {}
	m.infos = newInfoCache(func(pid int) (sys.ProcInfo, bool) {
		ow, ok := rd.Owner(pid)
		return sys.ProcInfo{Agent: ow.Agent, Session: ow.Session, AgentPID: ow.AgentPID}, ok
	})
	m.infos.direct = true
	m.footprint = rd.Footprint
	cfgSet := o.Config != nil
	if cfgSet {
		m.cfg = *o.Config
	}
	m.cfg.DryRun = false // take every decision, so each can be scored

	rep := &ReplayReport{Policy: "agentrm"}
	taken := map[string]*ReplayStop{} // resource ID -> its stop, while stopped
	paused := map[string]time.Time{}
	gone := map[procKey]bool{} // processes the policy stopped: absent from what it sees next
	var liveDecisions []*model.Decision
	type replayDecision struct {
		action, res string
		at          time.Time
	}
	var decided []replayDecision

	var effMu sync.Mutex
	var effs []Effect
	old := Executor
	defer func() { Executor = old }()
	Executor = func(e Effect) error {
		effMu.Lock()
		effs = append(effs, e)
		effMu.Unlock()
		return nil
	}
	drain := func() []Effect {
		effMu.Lock()
		defer effMu.Unlock()
		out := effs
		effs = nil
		return out
	}
	m.onDecide = func(d *model.Decision) {
		decided = append(decided, replayDecision{d.Action, d.Resource, d.Time})
		if debugReplay != nil {
			debugReplay("replay %s %s at %s: %s", d.Action, d.Resource, d.Time.Format("15:04:05"), d.Why)
		}
	}

	// filter hides what the policy has stopped: in a world where it was stopped, it is gone.
	filter := func(procs map[int]*sys.Proc, found []*model.Resource) (map[int]*sys.Proc, []*model.Resource) {
		if len(gone) == 0 && len(taken) == 0 {
			return procs, found
		}
		children := childrenOf(procs)
		out := make(map[int]*sys.Proc, len(procs))
		for pid, p := range procs {
			out[pid] = p
		}
		for _, p := range procs {
			if gone[keyOf(p)] {
				stack := []int{p.PID}
				for len(stack) > 0 {
					pid := stack[len(stack)-1]
					stack = stack[:len(stack)-1]
					delete(out, pid)
					stack = append(stack, children[pid]...)
				}
			}
		}
		var keep []*model.Resource
		for _, r := range found {
			if taken[r.ID] == nil {
				keep = append(keep, r)
			}
		}
		return out, keep
	}

	type qkey struct{ session, kind string }
	queuedAt := map[qkey]time.Time{}
	var waits []time.Duration
	watchQueue := func(now time.Time) {
		for _, w := range m.waiters {
			k := qkey{w.Session, w.Kind}
			if _, ok := queuedAt[k]; !ok {
				queuedAt[k] = w.Since
				rep.Queued++
			}
		}
		for k, at := range queuedAt {
			if m.findLease(k.session, k.kind) != nil {
				waits = append(waits, now.Sub(at))
				rep.Granted++
				delete(queuedAt, k)
			}
		}
	}

	var lastProcs map[int]*sys.Proc
	for {
		f, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		rep.Frames++
		if rep.From.IsZero() {
			rep.From = f.T
		}
		rep.To = f.T
		clk.t = f.T
		switch {
		case f.Sweep != nil:
			if !cfgSet && f.Sweep.Mem.TotalMB > 0 && m.mem.TotalMB == 0 {
				model.ScaleToMachine(&m.cfg, f.Sweep.Mem.TotalMB)
			}
			procs, found := filter(rd.Table(), f.Sweep.Found)
			lastProcs = procs
			failed := map[string]error{}
			for src, msg := range f.Sweep.Failed {
				failed[src] = errors.New(msg)
			}
			m.readFast = func() sys.MemStats { return m.mem }
			m.sweepWith(procs, discover.Result{Resources: cloneResources(found), Failed: failed}, f.Sweep.Mem, f.Sweep.Est, f.Sweep.Cwds)
		case f.Fast != nil:
			procs, _ := filter(rd.Table(), nil)
			lastProcs = procs
			m.fastWith(procs, f.Fast.Mem)
		case f.Request != nil:
			r := f.Request
			req := r.Req
			req.WaitSeconds = 0
			h, ok := m.routes()[r.Path]
			if !ok {
				continue
			}
			fx := facts{pid: r.Facts.PID, agentPID: r.Facts.AgentPID, agent: r.Facts.Agent, gui: r.Facts.GUI, app: r.Facts.App, viaApp: r.Facts.ViaApp}
			if r.Path == "/approve" || r.Path == "/config" || r.Path == "/priority" || r.Path == "/review" {
				continue // a person's settings and marks: not part of the policy
			}
			_, _ = m.handleFacts(context.Background(), r.Path, h, fx, req)
		case f.Exit > 0:
			procs, _ := filter(rd.Table(), nil)
			lastProcs = procs
			m.AgentExited(f.Exit, procs)
		case f.Origin != nil:
			m.mu.Lock()
			m.dockerOrigin[f.Origin.ID] = origin{session: f.Origin.Session, at: f.T}
			m.mu.Unlock()
		case f.Decision != nil:
			liveDecisions = append(liveDecisions, f.Decision)
		}
		// Effects carried out this frame.
		for _, e := range drain() {
			switch e.Op {
			case "pause":
				paused[e.Res.ID] = f.T
				rep.Pauses++
				if u := truth.after(truth.byID[e.Res.ID], f.T, time.Hour); !u.IsZero() {
					rep.PausesUsed++
				}
			case "resume":
				delete(paused, e.Res.ID)
			case "stop":
				st := &ReplayStop{Op: "stop", ID: e.Res.ID, Name: e.Res.Name, MemMB: e.Res.MemMB, At: f.T, Why: e.Why}
				st.UsedAt = truth.after(truth.byID[e.Res.ID], f.T, o.ReuseWindow)
				if lastProcs != nil && e.Res.PID > 0 {
					if p := lastProcs[e.Res.PID]; p != nil {
						gone[keyOf(p)] = true
						if u := truth.after(truth.byPID[keyOf(p)], f.T, o.ReuseWindow); !u.IsZero() && (st.UsedAt.IsZero() || u.Before(st.UsedAt)) {
							st.UsedAt = u
						}
						if _, _, alive, ok := m.envOwner(p, lastProcs); ok && alive {
							st.LiveOwner = true
						}
					}
				}
				taken[e.Res.ID] = st
				rep.Stops = append(rep.Stops, st)
			}
		}
		watchQueue(f.T)
	}
	finishStops(rep)
	if p := m.st.Predict; p != nil {
		rep.Predicted, rep.PredictHits, rep.BaselineHits = p.Total, p.Hits, p.BaseHits
	}
	if n := len(waits); n > 0 {
		var sum time.Duration
		for _, w := range waits {
			sum += w
			rep.WaitMax = max(rep.WaitMax, w)
		}
		rep.WaitMean = sum / time.Duration(n)
	}
	// Agreement with the live daemon's decisions.
	rep.LiveDecisions = len(liveDecisions)
	if len(liveDecisions) > 0 {
		hit := 0
		for _, d := range liveDecisions {
			for _, r := range decided {
				if r.action == d.Action && r.res == d.Resource && absDur(r.at.Sub(d.Time)) <= time.Minute {
					hit++
					break
				}
			}
		}
		rep.Agreement = float64(hit) / float64(len(liveDecisions))
	}
	return rep, nil
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func cloneResources(rs []*model.Resource) []*model.Resource {
	out := make([]*model.Resource, len(rs))
	for i, r := range rs {
		c := *r
		out[i] = &c
	}
	return out
}

// finishStops totals what the stops freed and how many were wrong.
func finishStops(rep *ReplayReport) {
	for _, s := range rep.Stops {
		rep.StopsN++
		until := rep.To
		if s.Wrong() {
			rep.WrongStops++
			until = s.UsedAt
		}
		if s.LiveOwner {
			rep.LiveOwnerStops++
		}
		rep.FreedGBh += s.MemMB / 1024 * until.Sub(s.At).Hours()
	}
}

// ---- the reaper, for comparison ----

// reaperNames is what the cleanup scripts look for among processes re-parented to launchd.
var reaperNames = regexp.MustCompile(`^(node|nodejs|npm|npx|pnpm|yarn|bun|deno|python[0-9.]*|uvicorn|gunicorn|ruby|rails|java|go|air|vite|next|esbuild|tsc|webpack|postgres|redis-server|mongod|mysqld)$`)

func replayReaper(path string, truth *usage, o ReplayOptions) (*ReplayReport, error) {
	rd, err := trace.Open(path)
	if err != nil {
		return nil, err
	}
	rep := &ReplayReport{Policy: "reaper"}
	killed := map[procKey]bool{}
	for {
		f, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		rep.Frames++
		if rep.From.IsZero() {
			rep.From = f.T
		}
		rep.To = f.T
		if f.Sweep == nil {
			continue
		}
		procs := rd.Table()
		children := childrenOf(procs)
		for _, p := range procs {
			k := keyOf(p)
			if killed[k] || p.PPID != 1 || !reaperNames.MatchString(filepath.Base(p.Exe)) {
				continue
			}
			killed[k] = true
			mb := 0.0
			stack := []int{p.PID}
			for len(stack) > 0 {
				pid := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				if q := procs[pid]; q != nil {
					fp, _ := rd.Footprint(pid)
					mb += max(q.RSSMB, fp)
					stack = append(stack, children[pid]...)
				}
			}
			st := &ReplayStop{Op: "kill", ID: "pid:" + strconv.Itoa(p.PID), Name: filepath.Base(p.Exe), MemMB: mb, At: f.T,
				Why: "re-parented to launchd and named like a dev process"}
			st.UsedAt = truth.after(truth.byPID[k], f.T, o.ReuseWindow)
			for _, r := range f.Sweep.Found {
				if r.PID == p.PID {
					if u := truth.after(truth.byID[r.ID], f.T, o.ReuseWindow); !u.IsZero() && (st.UsedAt.IsZero() || u.Before(st.UsedAt)) {
						st.UsedAt = u
					}
				}
			}
			// Was the agent that started it still running? (Then it was killed from under a live session.)
			if ow, ok := rd.Owner(p.PID); ok && ow.AgentPID > 0 {
				if ap := procs[ow.AgentPID]; ap != nil && sys.AgentKind(ap) != "" {
					st.LiveOwner = true
				}
			}
			rep.Stops = append(rep.Stops, st)
		}
	}
	finishStops(rep)
	return rep, nil
}

// String is the report for a terminal.
func (r *ReplayReport) String() string {
	s := fmt.Sprintf("%s over %s (%s to %s, %d frames)\n", r.Policy, r.To.Sub(r.From).Round(time.Minute),
		r.From.Format("Jan 2 15:04"), r.To.Format("Jan 2 15:04"), r.Frames)
	wrongPct := 0.0
	if r.StopsN > 0 {
		wrongPct = 100 * float64(r.WrongStops) / float64(r.StopsN)
	}
	s += fmt.Sprintf("  stopped %d, wrong %d (%.0f%%), %d while their agent was still running\n", r.StopsN, r.WrongStops, wrongPct, r.LiveOwnerStops)
	s += fmt.Sprintf("  freed %.1f GB-hours\n", r.FreedGBh)
	if r.Policy == "agentrm" {
		s += fmt.Sprintf("  paused %d, used while paused %d\n", r.Pauses, r.PausesUsed)
		s += fmt.Sprintf("  queued %d, granted %d, wait mean %s, max %s\n", r.Queued, r.Granted, r.WaitMean.Round(time.Second), r.WaitMax.Round(time.Second))
		if r.Predicted > 0 {
			s += fmt.Sprintf("  next heavy command predicted %d/%d, baseline %d/%d\n", r.PredictHits, r.Predicted, r.BaselineHits, r.Predicted)
		}
		if r.LiveDecisions > 0 {
			s += fmt.Sprintf("  agreement with the live daemon: %.0f%% of %d decisions\n", 100*r.Agreement, r.LiveDecisions)
		}
	}
	for _, st := range r.Stops {
		if st.Wrong() {
			s += fmt.Sprintf("  WRONG %s %s (%s, %.0f MB) at %s, used again %s later\n", st.Op, st.ID, st.Name, st.MemMB,
				st.At.Format("15:04"), st.UsedAt.Sub(st.At).Round(time.Minute))
		}
	}
	return s
}
