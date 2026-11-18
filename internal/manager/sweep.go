package manager

import (
	"bufio"
	"context"
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"agentrm/internal/discover"
	"agentrm/internal/evidence"
	"agentrm/internal/model"
	"agentrm/internal/sys"
	"agentrm/internal/trace"
)

// Run samples the machine until ctx is done. Two tiers:
//
//   - a full sweep (discovery, attribution, leases, orphans) every SweepSeconds, or at once
//     when asked (a lease was granted, a request is waiting);
//   - a fast tick every FastSweepSeconds, only while something needs it (a build or inferred
//     heavy lease, a waiting request, a paused server to thaw on connect): memory, pressure,
//     process trees and new connections. Cheap enough not to cost more than it saves.
func (m *Manager) Run(ctx context.Context) {
	go m.watchDocker(ctx)
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		n := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.persist()
				if n++; n%10 == 0 {
					if rec := m.recorder(); rec != nil {
						rec.Flush()
					}
				}
			}
		}
	}()
	m.Sweep()
	var exits <-chan int
	if m.exits != nil {
		exits = m.exits.C
	}
	full := time.NewTimer(m.sweepEvery())
	fast := time.NewTicker(time.Duration(max(1, m.cfg.FastSweepSeconds)) * time.Second)
	defer full.Stop()
	defer fast.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-full.C:
			m.Sweep()
			full.Reset(m.sweepEvery())
		case <-m.kick:
			m.Sweep()
			if !full.Stop() {
				select {
				case <-full.C:
				default:
				}
			}
			full.Reset(m.sweepEvery())
		case <-fast.C:
			if m.needsFast() {
				m.FastTick()
			}
		case pid := <-exits:
			// An agent exited: end its sessions now, with a table read after the exit.
			procs := sys.ListProcs()
			if rec := m.recorder(); rec != nil {
				now := m.now()
				rec.Write(trace.Frame{T: now, Exit: pid, ExitTab: rec.Procs(now, procs, m.traceOwner, m.footprint)})
			}
			m.AgentExited(pid, procs)
		}
	}
}

func (m *Manager) sweepEvery() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return time.Duration(max(5, m.cfg.SweepSeconds)) * time.Second
}

// kickSweep asks for a full sweep soon. Must not block (callers may hold mu).
func (m *Manager) kickSweep() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

func (m *Manager) needsFast() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.liveWaiters(m.now())) > 0 {
		return true
	}
	for _, l := range m.st.Leases {
		if len(l.Resources) == 0 && (model.BuildKinds[l.Kind] || l.Kind == model.KHeavy) {
			return true
		}
	}
	for _, r := range m.st.Resources {
		if r.PausedAt != nil && len(r.Ports) > 0 && m.cfg.AutoThawOnConnect {
			return true
		}
	}
	// An agent with a big tree and no lease may be starting something heavy.
	for _, s := range m.st.Sessions {
		if s.FootprintMB > m.cfg.HeavyGrowthGB*1024/2 {
			return true
		}
	}
	return false
}

// FastTick refreshes memory, pressure and process trees, and thaws paused servers someone is
// trying to reach.
func (m *Manager) FastTick() {
	procs := sys.ListProcs()
	mem := sys.ReadMem()
	if rec := m.recorder(); rec != nil {
		now := m.now()
		rec.Write(trace.Frame{T: now, Fast: &trace.Sweep{Procs: rec.Procs(now, procs, m.traceOwner, m.footprint), Mem: mem}})
	}
	m.fastWith(procs, mem)
}

// fastWith is a fast tick on what was read (live, or from a trace).
func (m *Manager) fastWith(procs map[int]*sys.Proc, mem sys.MemStats) {
	m.mu.Lock()
	now := m.now()
	m.takeMem(mem, now)
	if procs != nil {
		sys.Procs.Set(procs)
		m.updateFootprints(procs, now)
	}
	m.grantWaiters(now, false) // memory may have just come free
	var watch []string
	paused := map[string]*model.Resource{}
	if m.cfg.AutoThawOnConnect {
		for _, r := range m.st.Resources {
			if r.PausedAt != nil && m.pending[r.ID] == "" {
				for _, p := range r.Ports {
					watch = append(watch, p)
					paused[p] = r
				}
			}
		}
	}
	m.mu.Unlock()
	if len(watch) == 0 {
		return
	}
	tuples := sys.ConnTuples(watch)
	m.mu.Lock()
	var effects []Effect
	thawed := map[string]bool{}
	for port, r := range paused {
		seen := m.connSeen[r.ID]
		if seen == nil {
			seen = map[string]bool{}
			m.connSeen[r.ID] = seen
		}
		for t := range tuples[port] {
			if !seen[t] {
				seen[t] = true
				if !thawed[r.ID] && m.st.Resources[r.ID] != nil && m.pending[r.ID] == "" {
					thawed[r.ID] = true
					effects = append(effects, m.thawEffect(r, "something connected to port "+port))
				}
			}
		}
	}
	m.mu.Unlock()
	if len(effects) > 0 {
		m.afterAsync(RunEffects(effects))
	}
}

func (m *Manager) takeMem(mem sys.MemStats, now time.Time) {
	if !m.lastSwapAt.IsZero() && now.Sub(m.lastSwapAt) > time.Second && mem.Swapouts >= m.lastSwapouts {
		m.swapRate = (mem.Swapouts - m.lastSwapouts) / now.Sub(m.lastSwapAt).Minutes()
	}
	m.lastSwapouts, m.lastSwapAt = mem.Swapouts, now
	m.mem = mem
	m.measureReturns(now)
	m.watchGrants(now)
}

// Sweep is the full pass: discovery, attribution, leases, orphans.
func (m *Manager) Sweep() {
	start := time.Now()
	procs := sys.ListProcs()
	if procs != nil {
		sys.Procs.Set(procs)
	}
	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()
	found := discover.Discover(cfg, procs)
	m.infos.refresh(procs) // read new processes' environments outside mu
	m.mu.Lock()
	needCwd := m.leftoverCwdsNeeded()
	m.mu.Unlock()
	var cwds map[int]string
	if len(needCwd) > 0 {
		cwds = sys.Cwds(needCwd)
	}
	mem := sys.ReadMem()
	est := sys.EstablishedPorts()
	m.tamperCheck()
	m.recordSweep(procs, found, mem, est, cwds)
	m.sweepWith(procs, found, mem, est, cwds)
	self := sys.Self()
	self.SweepMS = float64(time.Since(start).Microseconds()) / 1000
	m.mu.Lock()
	m.self = self
	m.mu.Unlock()
}

// recordSweep writes a sweep's inputs to the trace, if one is being kept.
func (m *Manager) recordSweep(procs map[int]*sys.Proc, found discover.Result, mem sys.MemStats, est map[string]bool, cwds map[int]string) {
	rec := m.recorder()
	if rec == nil {
		return
	}
	m.infos.refresh(procs) // so the trace has each process's owner from its first frame
	now := m.now()
	failed := map[string]string{}
	for src, err := range found.Failed {
		failed[src] = err.Error()
	}
	rec.Write(trace.Frame{T: now, Sweep: &trace.Sweep{Procs: rec.Procs(now, procs, m.traceOwner, m.footprint),
		Found: found.Resources, Failed: failed, Mem: mem, Est: est, Cwds: cwds}})
}

// sweepWith is a full sweep on what was read (live, or from a trace).
func (m *Manager) sweepWith(procs map[int]*sys.Proc, found discover.Result, mem sys.MemStats, est map[string]bool, cwds map[int]string) {
	m.mu.Lock()
	now := m.now()
	m.takeMem(mem, now)
	m.lastSweep = now
	if procs != nil {
		m.updateFootprints(procs, now)
		m.checkLiveness(procs, now)
		m.setLeftoverCwds(cwds)
		found.Resources = m.withLeftovers(found.Resources, procs)
	}
	m.merge(found, procs, est, now)
	m.checkLeases(now)
	if procs != nil {
		m.watchLeaks(procs, now) // after merge and the leases: what explains growth is current
	}
	m.judgeYields(now)
	m.markExpected(now)
	m.weeklyReceipt(now)
	effects := m.checkOrphans(now)
	m.grantWaiters(now, true)
	for id, s := range m.st.Sessions {
		if now.Sub(s.LastSeen) > 24*time.Hour && s.FootprintMB == 0 {
			delete(m.st.Sessions, id)
			delete(m.footBase, id)
			delete(m.recent, id)
			delete(m.st.Trees, id)
			delete(m.missing, id)
			if m.st.Predict != nil {
				delete(m.st.Predict.Last, id)
			}
		}
	}
	m.prune(now)
	for id, o := range m.origins() {
		if now.Sub(o.at) > time.Hour {
			delete(m.dockerOrigin, id)
		}
	}
	m.dirty = true
	effects = m.gateAuto(effects)
	m.mu.Unlock()

	if len(effects) > 0 {
		m.afterAsync(RunEffects(effects))
	}
}

func (m *Manager) origins() map[string]origin { return m.dockerOrigin }

// merge folds a discovery result into the state. A source that failed keeps its last view:
// concluding its resources are gone would drop their leases and restart their clocks (D4).
func (m *Manager) merge(found discover.Result, procs map[int]*sys.Proc, est map[string]bool, now time.Time) {
	m.failed = found.Failed
	seen := map[string]bool{}
	for _, r := range found.Resources {
		seen[r.ID] = true
		old := m.st.Resources[r.ID]
		if old != nil {
			r.FirstSeen, r.LeaseID, r.OrphanSince = old.FirstSeen, old.LeaseID, old.OrphanSince
			r.LastActive = old.LastActive
			if r.StartedBy == "" {
				r.StartedBy = old.StartedBy
			}
			if r.Owner == "" {
				r.Owner, r.OwnerGoneAt = old.Owner, old.OwnerGoneAt
			}
			r.PausedAt, r.Urgent, r.ReleasedIdle = old.PausedAt, old.Urgent, old.ReleasedIdle && r.LeaseID == ""
			if r.Cwd == "" {
				r.Cwd = old.Cwd
			}
		} else {
			r.FirstSeen = now
			m.yieldsSeeReturn(r, now)
		}
		if r.LeaseID != "" && m.st.Leases[r.LeaseID] == nil {
			r.LeaseID = ""
		}
		r.Pinned = m.st.Pinned[r.ID]
		m.st.Resources[r.ID] = r
		if r.LeaseID == "" {
			// Ancestry and Docker events need no history, so they are tried on every sweep (and
			// after a restart); what recent commands say only counts for something new.
			m.attribute(r, procs, now, old == nil && m.firstDone)
		}
		if r.PausedAt == nil && m.activity.Active(r, est) {
			last := r.LastActive
			if last.IsZero() {
				last = r.FirstSeen
			}
			m.noteIdleGap(r.Kind, now.Sub(last).Minutes())
			r.LastActive = now
		}
	}
	for id, r := range m.st.Resources {
		if found.Failed[r.Source] != nil && !seen[id] {
			// Kept as last seen, and marked so: it may well be gone (D4).
			if r.Unseen == nil {
				t := now
				r.Unseen = &t
			}
			continue
		}
		if seen[id] || m.pending[id] != "" {
			continue
		}
		m.noteGone(r, now)
		delete(m.st.Resources, id)
		delete(m.st.Questions, id)
		delete(m.st.Touches, id)
		delete(m.connSeen, id)
		m.activity.Forget(id)
		for _, l := range m.st.Leases {
			l.Resources = remove(l.Resources, id)
		}
	}
	m.firstDone = true
}

// attribute binds a resource that has no lease to the session that started it.
func (m *Manager) attribute(r *model.Resource, procs map[int]*sys.Proc, now time.Time, isNew bool) {
	s := m.startedBy(r, procs, now, isNew)
	if ended := m.sessionEnded(s); ended != nil {
		// Its starter is gone: it is not a root, so it keeps nothing alive. Credit it and let
		// the collector have it.
		r.StartedBy = s
		if r.Owner == "" {
			t := *ended
			r.Owner, r.OwnerGoneAt = s, &t
		}
		return
	}
	if s != "" {
		r.StartedBy = s
		var best *model.Lease
		for _, l := range m.st.Leases {
			if l.Session == s && model.LeaseToResource[l.Kind] == r.Kind && (best == nil || l.Created.After(best.Created)) {
				best = l
			}
		}
		if best == nil {
			agent, ev := agentOfSession(s), "this session started it"
			if s == "you" {
				ev = "you started it (no agent was involved)"
			}
			best = &model.Lease{ID: newID(), Session: s, Agent: agent, Kind: model.ResourceToLease[r.Kind], MemGB: r.MemMB / 1024,
				Purpose: "started without a lease", Cwd: r.Cwd, TTL: m.cfg.DefaultTTLMinutes, Created: now,
				Expires: now.Add(minutes(m.cfg.DefaultTTLMinutes)), Verified: true, Evidence: ev, LastUsed: now}
			m.stampTask(best)
			m.st.Leases[best.ID] = best
			if s == "you" {
				best.Purpose = "started by you"
				m.event("YOURS %s was started by you; recorded as lease %s", r.ID, best.ID)
			} else {
				m.event("UNLEASED %s was started by %s without a lease; recorded as lease %s", r.ID, short(s), best.ID)
			}
		}
		r.LeaseID = best.ID
		best.Resources = append(best.Resources, r.ID)
		m.event("BIND %s to lease %s (%s started it)", r.ID, best.ID, short(s))
		return
	}
	if !isNew {
		return
	}
	// No evidence: bind only if exactly one lease is still waiting for a resource like this.
	var cands []*model.Lease
	for _, l := range m.st.Leases {
		if model.LeaseToResource[l.Kind] == r.Kind && now.Sub(l.Created) < 15*time.Minute && len(l.Resources) == 0 {
			cands = append(cands, l)
		}
	}
	if len(cands) == 1 {
		r.LeaseID = cands[0].ID
		if r.StartedBy == "" {
			r.StartedBy = cands[0].Session
		}
		cands[0].Resources = append(cands[0].Resources, r.ID)
		m.event("BIND %s to lease %s (%s; only candidate)", r.ID, cands[0].ID, short(cands[0].Session))
	}
}

// startedBy finds the session that started r: by the Docker event stream, by process ancestry,
// or (for something just seen) by the recent command that named it. If no agent can have
// started it, it is yours.
func (m *Manager) startedBy(r *model.Resource, procs map[int]*sys.Proc, now time.Time, isNew bool) string {
	if o, ok := m.dockerOrigin[r.ID]; ok && o.session != "" {
		return o.session
	}
	if r.PID > 0 && procs != nil {
		if apid, kind := sys.AgentAncestor(procs, r.PID); apid > 0 {
			return m.sessionOf(apid, kind)
		}
		if p := procs[r.PID]; p != nil {
			if gui, _, _ := sys.GUIAncestor(procs, p.PPID); gui {
				return "you" // started from your terminal or IDE, with no agent above it
			}
		}
		// Detached (Gradle daemons always are): its environment says which agent session
		// started it, if one did.
		if p := procs[r.PID]; p != nil {
			if session, _, _, ok := m.envOwner(p, procs); ok {
				return session
			}
		}
	}
	if !isNew {
		return ""
	}
	single := map[string]*model.Resource{r.ID: r}
	best, bestAt := "", time.Time{}
	for session, cmds := range m.recent {
		for _, c := range cmds {
			if now.Sub(c.Time) > 10*time.Minute || !c.Time.After(bestAt) {
				continue
			}
			if len(evidence.TouchedBy(c.Cmd, c.Cwd, single, "")) > 0 {
				best, bestAt = session, c.Time
			}
		}
	}
	if best == "" && m.agentsQuiet(procs, now) {
		return "you"
	}
	return best
}

// agentsQuiet reports whether it is certain no agent started something that just appeared:
// no agent is running, or every running agent reports its commands through hooks and none has
// run one in the last 10 minutes.
func (m *Manager) agentsQuiet(procs map[int]*sys.Proc, now time.Time) bool {
	agents := 0
	for _, p := range procs {
		if k := sys.AgentKind(p); k != "" {
			// Hooked: it reports every command before running it (Claude Code always; another
			// agent once its hooks have been heard from today).
			if seen, ok := m.hookSeen[k]; !ok || now.Sub(seen) > 24*time.Hour {
				return false
			}
			agents++
		}
	}
	if agents == 0 {
		return true
	}
	for _, cmds := range m.recent {
		for _, c := range cmds {
			if now.Sub(c.Time) < 10*time.Minute {
				return false
			}
		}
	}
	return true
}

// ---- Docker events: who created a container, at the moment it is created ----

var dockerClients = map[string]bool{"docker": true, "com.docker.cli": true, "docker-compose": true,
	"kind": true, "supabase": true, "colima": true, "docker-buildx": true, "podman": true}

// watchDocker follows `docker events` and, for each container created, finds the client
// process that asked for it. This attributes containers started by scripts and Makefiles,
// which command text cannot.
func (m *Manager) watchDocker(ctx context.Context) {
	backoff := 30 * time.Second
	for ctx.Err() == nil {
		err := m.followDocker(ctx)
		m.mu.Lock()
		if err != nil {
			m.dockerEventsErr = err.Error()
		}
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Minute)
	}
}

func (m *Manager) followDocker(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "docker", "events", "--filter", "type=container", "--filter", "event=create",
		"--format", "{{json .}}")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	m.mu.Lock()
	m.dockerEventsErr = ""
	m.mu.Unlock()
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		var ev struct {
			Actor struct {
				Attributes map[string]string `json:"Attributes"`
			} `json:"Actor"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		a := ev.Actor.Attributes
		id, names := groupID(a)
		procs := sys.ListProcs() // now, while the client that asked is still running
		if s := originFromProcs(procs, names); s != "" {
			m.mu.Lock()
			m.dockerOrigin[id] = origin{session: m.resolveOrigin(s), at: m.now()}
			if rec := m.recorder(); rec != nil {
				f := trace.Frame{T: m.now()}
				f.Origin = &struct {
					ID      string `json:"id"`
					Session string `json:"session"`
				}{id, m.dockerOrigin[id].session}
				rec.Write(f)
			}
			m.mu.Unlock()
		}
	}
	_ = cmd.Wait()
	return errFromWait(sc.Err())
}

func errFromWait(err error) error {
	if err != nil {
		return err
	}
	return errDockerEventsEnded
}

type staticErr string

func (e staticErr) Error() string { return string(e) }

const errDockerEventsEnded = staticErr("docker events ended (Docker stopped?)")

// groupID is the resource ID discovery will give this container, and names that identify it.
func groupID(a map[string]string) (string, []string) {
	name := a["name"]
	switch {
	case a["io.x-k8s.kind.cluster"] != "":
		c := a["io.x-k8s.kind.cluster"]
		return "kind:" + c, []string{c, name}
	case a["com.supabase.cli.project"] != "":
		p := a["com.supabase.cli.project"]
		return "compose:supabase-" + p, []string{p, name}
	case a["com.docker.compose.project"] != "":
		p := a["com.docker.compose.project"]
		return "compose:" + p, []string{p, name}
	}
	return "docker:" + name, []string{name}
}

// originFromProcs finds which agent (or you) is running a Docker client right now. Clients
// that name the container win; if several sessions remain, it is ambiguous and nobody is
// credited. The result is "pid:kind:agentPID" for an agent, "you", or "".
func originFromProcs(procs map[int]*sys.Proc, names []string) string {
	type cand struct {
		who   string
		named bool
	}
	var cands []cand
	for _, p := range procs {
		base := p.Exe[strings.LastIndex(p.Exe, "/")+1:]
		if !dockerClients[base] {
			continue
		}
		named := false
		for _, n := range names {
			if n != "" && strings.Contains(p.Args, n) {
				named = true
			}
		}
		if apid, kind := sys.AgentAncestor(procs, p.PID); apid > 0 {
			cands = append(cands, cand{"pid:" + kind + ":" + strconv.Itoa(apid), named})
		} else if gui, _, _ := sys.GUIAncestor(procs, p.PPID); gui {
			cands = append(cands, cand{"you", named})
		}
	}
	pick := func(onlyNamed bool) string {
		who := ""
		for _, c := range cands {
			if onlyNamed && !c.named {
				continue
			}
			if who != "" && who != c.who {
				return "?"
			}
			who = c.who
		}
		return who
	}
	if w := pick(true); w != "" && w != "?" {
		return w
	}
	if w := pick(false); w != "?" {
		return w
	}
	return ""
}

// resolveOrigin turns originFromProcs' answer into a session name. Must hold mu.
func (m *Manager) resolveOrigin(s string) string {
	if !strings.HasPrefix(s, "pid:") {
		return s
	}
	parts := strings.SplitN(s, ":", 3)
	pid, err := strconv.Atoi(parts[2])
	if err != nil {
		return ""
	}
	return m.sessionOf(pid, parts[1])
}
