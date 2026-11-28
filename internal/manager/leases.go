package manager

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/model"
	"agentrm/internal/sys"
)

func (m *Manager) touchSession(id, agent, cwd string) *model.Session {
	if id == "" {
		return nil
	}
	s := m.st.Sessions[id]
	if s == nil {
		s = &model.Session{ID: id}
		m.st.Sessions[id] = s
	}
	if agent != "" {
		s.Agent = agent
	}
	if cwd != "" {
		s.Cwd = cwd
	}
	s.LastSeen = m.now()
	if s.EndedAt != nil && !strings.HasPrefix(s.EndReason, "cleared:") {
		m.reviveSession(s) // it is talking again: resumed, or its end was a false alarm
	}
	return s
}

func (m *Manager) acquire(c api.Caller, r api.Req) (any, []Effect, error) {
	if c.Session == "detached" {
		return nil, nil, fmt.Errorf("a detached process cannot take a lease; run it from an agent or a terminal")
	}
	if r.Kind == "" {
		return nil, nil, fmt.Errorf("kind is required (%s)", strings.Join(model.AllKinds, ", "))
	}
	now := m.now()
	cwd := r.Cwd
	if cwd == "" {
		if s := m.st.Sessions[c.Session]; s != nil {
			cwd = s.Cwd
		}
	}
	memGB := m.effectiveGB(r.Kind, cwd, r.MemGB, c.Session, now)
	// A session that already holds a lease of this kind reuses it. That is also how a lease
	// granted from the queue is taken up. Only for what the lease covers: asking for more goes
	// through admission for the difference (it used to be granted at the old size).
	// What the session holds of this kind is compared in total: asking for 2 GB again while
	// holding 1 + 1 is the same ask, not another 1 GB.
	var mine []*model.Lease
	heldGB, declared := 0.0, false
	for _, l := range m.st.Leases {
		if l.Session == c.Session && l.Kind == r.Kind {
			mine = append(mine, l)
			heldGB += l.MemGB
			declared = declared || l.Declared
		}
	}
	extraGB := 0.0
	if len(mine) > 0 {
		// The hook's automatic request declares nothing; it must not second-guess a lease
		// the agent took with its own figure.
		if more := memGB - heldGB; more > 0.25 && (r.MemGB > 0 || !declared) {
			extraGB = more
		} else {
			// Reused: all of them are renewed and taken up; the answer names the one granted from
			// the queue if there is one, else the newest.
			sort.Slice(mine, func(i, j int) bool { return mine[i].Created.After(mine[j].Created) })
			pick := mine[0]
			for _, l := range mine {
				if l.Offered != nil && pick.Offered == nil {
					pick = l
				}
				l.Expires = now.Add(minutes(l.TTL))
				l.LastUsed = now
				m.takeUp(l)
			}
			m.dropWaiter(c.Session, r.Kind)
			return &api.AcquireResp{Granted: true, Lease: pick, Reused: true}, nil, nil
		}
	}
	if extraGB > 0 {
		memGB = extraGB // another lease of this kind for the difference
	}
	m.yieldsSeeUse(c.Session, nil, r.Kind, now)
	fast := m.readFast() // pressure, load and disk right now, not as of the last sweep
	m.mem.PressureLevel, m.mem.Load1, m.mem.Cores, m.mem.DiskFreeGB = fast.PressureLevel, fast.Load1, fast.Cores, fast.DiskFreeGB
	m.mem.Thermal = fast.Thermal
	if why := m.busy(r.Kind); why != "" {
		m.noteHeatRefusal(c.Session)
		return &api.AcquireResp{Reason: why}, nil, nil
	}
	want := model.Waiter{Session: c.Session, Agent: c.Agent, Kind: r.Kind, NeedMB: memGB * 1024, HeldGB: heldGB * b2f(extraGB > 0),
		Purpose: r.Purpose, Cmd: trunc(r.Cmd, 200), Cwd: cwd, Via: r.Via,
		TTLMinutes: r.TTLMinutes, UntilMinutes: r.UntilMinutes, Declared: r.MemGB > 0}
	if why := m.failOpen(now); why != "" {
		l := m.grant(&want, now, " ("+why+")")
		m.dropWaiter(c.Session, r.Kind)
		return &api.AcquireResp{Granted: true, Lease: l}, nil, nil
	}
	if ok, why, pos := m.admit(want, now); !ok {
		m.kickSweep() // sample fast while someone waits
		if r.MemGB <= 0 {
			why += fmt.Sprintf(". That was AgentRM's estimate (%.1f GB). If you know this needs less, ask with your own figure: "+
				"agentrm acquire --kind %s --mem <GB> --wait (what past leases here really used is in %s)",
				memGB, r.Kind, m.receiptsPath(cwd))
		}
		return &api.AcquireResp{Reason: why + ". " + m.holders(), Queue: pos}, nil, nil
	}
	l := m.grant(&want, now, "")
	m.kickSweep() // bind what it starts as soon as it appears
	return &api.AcquireResp{Granted: true, Lease: l}, nil, nil
}

// grant makes the lease w asks for. Must hold mu.
func (m *Manager) grant(w *model.Waiter, now time.Time, note string) *model.Lease {
	if m.onGrant != nil {
		m.onGrant(w.NeedMB, m.headroomMB())
	}
	ttl := w.TTLMinutes
	if ttl == 0 {
		ttl = m.cfg.DefaultTTLMinutes
		if model.BuildKinds[w.Kind] {
			ttl = m.cfg.BuildTTLMinutes
		}
	}
	l := &model.Lease{ID: newID(), Session: w.Session, Agent: w.Agent, Kind: w.Kind, MemGB: w.NeedMB / 1024,
		Purpose: w.Purpose, Cmd: w.Cmd, Cwd: w.Cwd, TTL: ttl, Created: now,
		Expires: now.Add(minutes(ttl)), Verified: true, Evidence: "requested before starting", LastUsed: now,
		Declared: w.Declared}
	if w.UntilMinutes > 0 {
		u := now.Add(minutes(w.UntilMinutes))
		l.Until = &u
	}
	if s := m.st.Sessions[w.Session]; s != nil {
		l.BaseFootprintMB = s.FootprintMB
		for len(s.Grants) > 0 && now.Sub(s.Grants[0]) > 24*time.Hour {
			s.Grants = s.Grants[1:]
		}
		s.Grants = append(s.Grants, now)
	}
	switch host := daemonHost(l.Kind); host {
	case model.RKDockerVM:
		l.DaemonBaseMB = map[string]float64{dockerRebaseKey: 1} // the first measurement is its base
		if m.dockerMeasured {
			l.DaemonBaseMB = m.dockerBase()
		}
	case "":
	default:
		l.DaemonBaseMB = map[string]float64{}
		for id, r := range m.st.Resources {
			if r.Kind == host {
				l.DaemonBaseMB[id] = r.MemMB
			}
		}
	}
	m.stampTask(l)
	if m.lastBackfill {
		l.Backfill, m.lastBackfill = true, false
		note += " (backfilled ahead of a larger waiting request)"
	}
	m.st.Leases[l.ID] = l
	m.event("GRANT %s %s %.1f GB to %s (%s)%s: %s", l.ID, l.Kind, l.MemGB, w.Session, w.Agent, note, l.Purpose)
	m.grantProbes = append(m.grantProbes, &grantProbe{at: now, lease: l.ID})
	return l
}

func (m *Manager) dropLease(l *model.Lease, why string) {
	m.learn(l)
	m.learnLifetime(l, m.now())
	delete(m.st.Questions, "idle:"+l.ID)
	delete(m.st.Questions, "grant:"+l.ID)
	idle := strings.HasPrefix(why, "idle for")
	for _, rid := range l.Resources {
		if r := m.st.Resources[rid]; r != nil && r.LeaseID == l.ID {
			r.LeaseID, r.ReleasedIdle = "", idle
		}
	}
	delete(m.st.Leases, l.ID)
	m.event("RELEASE %s %s (%s): %s", l.ID, l.Kind, short(l.Session), why)
}

func (m *Manager) release(c api.Caller, r api.Req) (any, []Effect, error) {
	if r.LeaseID != "" {
		l := m.st.Leases[r.LeaseID]
		if l == nil {
			return nil, nil, fmt.Errorf("no lease %s", r.LeaseID)
		}
		if l.Session != c.Session {
			if !c.Human {
				return nil, nil, fmt.Errorf("lease %s belongs to %s; an agent can release only its own leases", l.ID, l.Session)
			}
			if err := m.privileged(c, "release lease "+l.ID+" of "+l.Session); err != nil {
				return nil, nil, err
			}
		}
		m.dropLease(l, "released by "+c.Session)
		m.grantWaiters(m.now(), false) // what it was promised can go to the queue at once
		return &api.CountResp{N: 1}, nil, nil
	}
	if c.Session == "detached" {
		return nil, nil, fmt.Errorf("a detached process has no leases")
	}
	n := 0
	for _, l := range m.st.Leases {
		if l.Session == c.Session && (r.Kind == "" || l.Kind == r.Kind) {
			m.dropLease(l, "released by "+c.Session)
			n++
		}
	}
	if n > 0 {
		m.grantWaiters(m.now(), false)
	}
	return &api.CountResp{N: n}, nil, nil
}

// renew keeps a session's waiting leases alive. Leases that hold resources renew on use (see
// checkLeases), not on any tool call.
func (m *Manager) renew(c api.Caller, r api.Req) (any, []Effect, error) {
	n := 0
	now := m.now()
	for _, l := range m.st.Leases {
		if l.Session == c.Session && !model.BuildKinds[l.Kind] && len(l.Resources) == 0 {
			l.Expires = now.Add(minutes(l.TTL))
			n++
		}
	}
	// The hook's post-tool call releases a build lease when its command has returned.
	if r.Via == "hook" && r.Cmd != "" && !r.RunInBackground {
		if kind := m.classifier.Classify(r.Cmd); model.BuildKinds[kind] {
			for _, l := range m.st.Leases {
				if l.Session == c.Session && l.Kind == kind {
					m.dropLease(l, "its command finished")
				}
			}
		}
	}
	return &api.CountResp{N: n}, nil, nil
}

// checkLeases keeps resource-holding leases alive on use, holds declared end times, and asks
// holders about idle leases. Must be called with mu held.
func (m *Manager) checkLeases(now time.Time) {
	grace := minutes(m.cfg.GraceMinutes)
	idle := minutes(m.cfg.IdleMinutes)
	for _, l := range m.st.Leases {
		if l.Offered != nil && (len(l.Resources) > 0 || m.startedMB(l) >= 50) {
			m.takeUp(l) // something started under it
		}
		switch {
		case now.After(l.Expires):
			m.dropLease(l, "expired (not renewed or released)")
			continue
		case l.Offered != nil && now.Sub(*l.Offered) > m.grantHold():
			m.dropLease(l, fmt.Sprintf("granted from the queue but not taken up in %d min", int(m.grantHold().Minutes())))
			continue
		case !model.BuildKinds[l.Kind] && l.Kind != model.KHeavy && len(l.Resources) == 0 &&
			now.Sub(l.Created) > 15*time.Minute && m.startedMB(l) < 50:
			m.dropLease(l, "granted 15 min ago but nothing was started under it")
			continue
		}
		// A declared end time is held: past it, the holder is asked, and no answer releases it.
		if l.Until != nil && now.After(*l.Until) {
			if l.IdleAsked == nil {
				t := now
				l.IdleAsked = &t
				m.askHolder(l, now, now.Add(grace), fmt.Sprintf("You said you needed it until %s;", l.Until.Format("15:04")))
			} else if now.Sub(*l.IdleAsked) > grace {
				m.dropLease(l, "its declared end time passed and the holder did not extend it")
			}
			continue
		}
		if len(l.Resources) == 0 {
			continue
		}
		used, mem := l.LastUsed, 0.0
		for _, rid := range l.Resources {
			r := m.st.Resources[rid]
			if r == nil {
				continue
			}
			mem += r.MemMB
			if r.LastActive.After(used) {
				used = r.LastActive
			}
			if t := m.st.Touches[rid][l.Session]; t.After(used) {
				used = t
			}
		}
		l.PeakMB = max(l.PeakMB, mem)
		l.HadResources = true
		m.checkOverrun(l, now)
		if m.checkTaskDone(l, used, now) {
			continue
		}
		if used.After(l.LastUsed) {
			l.LastUsed, l.IdleAsked = used, nil
			delete(m.st.Questions, "idle:"+l.ID)
		}
		if exp := l.LastUsed.Add(minutes(l.TTL)); exp.After(l.Expires) {
			l.Expires = exp
		}
		if now.Sub(l.LastUsed) < idle && l.IdleAsked == nil {
			continue
		}
		if l.IdleAsked == nil {
			t := now
			l.IdleAsked = &t
			m.askHolder(l, now, now.Add(grace), "FYI,")
		} else if now.Sub(*l.IdleAsked) > grace {
			m.dropLease(l, fmt.Sprintf("idle for %s and no reply", now.Sub(l.LastUsed).Round(time.Minute)))
		}
	}
}

// askHolder puts the idle question to a lease's holder (and to you, for your own leases).
func (m *Manager) askHolder(l *model.Lease, now, deadline time.Time, lead string) {
	what := strings.Join(l.Resources, ", ")
	if what == "" {
		what = l.Kind
	}
	target := l.ID
	if len(l.Resources) > 0 {
		target = l.Resources[0]
	}
	m.st.Questions["idle:"+l.ID] = &model.Question{ResourceID: strings.Join(l.Resources, ","), Created: now, For: l.Session,
		Delivered: map[string]bool{}, Answers: map[string]string{},
		Text: fmt.Sprintf("[agentrm] %s your lease %s on %s has not been used for %s. Still need it? Run "+
			"agentrm claim %s --purpose \"<why>\" to keep it, or agentrm release %s. "+
			"No answer by %s releases it.", lead, l.ID, what, now.Sub(l.LastUsed).Round(time.Minute),
			target, l.ID, deadline.Format("15:04"))}
	if l.Session == "you" {
		m.event("ASKYOU %s, which you started, has not been used for %s. It is released at %s unless you claim it in AgentRM.",
			what, now.Sub(l.LastUsed).Round(time.Minute), deadline.Format("15:04"))
		m.notify("Still using "+what+"?", "Unused for "+now.Sub(l.LastUsed).Round(time.Minute).String()+
			". Released at "+deadline.Format("15:04")+" unless you claim it.")
		return
	}
	m.event("IDLE lease %s (%s) unused for %s: asking %s; deadline %s", l.ID, what,
		now.Sub(l.LastUsed).Round(time.Minute), short(l.Session), deadline.Format("15:04"))
}

// ---- process trees: what each agent really uses ----

// updateFootprints measures each live agent session's process tree, feeds build leases (which
// hold no discoverable resource), and turns unexplained growth into a lease of its own: the
// process tree, not the command text, is the primary sign of heavy work. Must hold mu.
func (m *Manager) updateFootprints(procs map[int]*sys.Proc, now time.Time) {
	live := map[string]bool{}
	trees := sys.AgentTrees(procs)
	for apid, tree := range trees {
		mb := 0.0
		for _, p := range tree {
			mb += m.procMB(p)
		}
		kind := sys.AgentKind(procs[apid])
		id := m.sessionOf(apid, kind)
		s := m.st.Sessions[id]
		if s == nil {
			s = &model.Session{ID: id, Agent: kind, LastSeen: now}
			m.st.Sessions[id] = s
		}
		if p := procs[apid]; p != nil && (s.AgentPID != apid || s.AgentStart.IsZero() || !sys.SameProcess(p, s.AgentStart)) {
			if s.EndedAt != nil && s.AgentPID == apid {
				// Same ID, new process (a PID reused by the same kind of agent): a new session.
				s.EndedAt, s.EndReason = nil, ""
				m.event("SESSION %s is a new %s process (pid %d)", short(id), kind, apid)
			}
			s.AgentStart = p.Start
		}
		if s.EndedAt != nil {
			continue // it said it ended; its process is on its way out
		}
		s.FootprintMB, s.AgentPID = mb, apid
		s.CPUPercent, s.BusyProcs = busiest(tree)
		live[id] = true
	}
	for id, s := range m.st.Sessions {
		if !live[id] {
			s.FootprintMB, s.CPUPercent, s.BusyProcs = 0, 0, nil
		}
	}
	explained := map[string]bool{} // sessions whose growth a lease accounts for
	// Processes that are resources held by a lease are that lease's, never a build's.
	bound := map[int]bool{}
	for _, r := range m.st.Resources {
		if r.LeaseID != "" && r.PID > 0 {
			bound[r.PID] = true
		}
	}
	builds := map[[2]string][]*model.Lease{}
	for _, l := range m.st.Leases {
		m.trackDockerBase(l) // every sweep, held containers or not, session seen or not
		if len(l.Resources) == 0 && !inferred(l) {
			builds[[2]string{l.Session, l.Kind}] = append(builds[[2]string{l.Session, l.Kind}], l)
		}
	}
	for key, ls := range builds {
		if s := m.st.Sessions[key[0]]; s != nil {
			m.measureBuilds(ls, trees[s.AgentPID], bound, now)
		}
	}
	for _, l := range m.st.Leases {
		if inferred(l) {
			l.UsedMB = m.materialisedMB(l) // the tree's growth: what the app shows for it
		}
		// Growth soon after a grant is that lease's work, whatever kind it is (a Gradle build
		// holds a daemon resource, and was being counted a second time as inferred heavy work).
		if len(l.Resources) == 0 || now.Sub(l.Created) < minutes(m.cfg.BuildTTLMinutes) {
			explained[l.Session] = true
		}
	}
	growth := m.cfg.HeavyGrowthGB * 1024
	for id, s := range m.st.Sessions {
		if !live[id] {
			continue
		}
		base, ok := m.footBase[id]
		if !ok {
			m.footBase[id] = s.FootprintMB
			continue
		}
		heavy := m.findLease(id, model.KHeavy)
		switch {
		case heavy != nil && s.FootprintMB < base+growth/3:
			m.dropLease(heavy, fmt.Sprintf("the process tree is back to %.1f GB", s.FootprintMB/1024))
		case heavy == nil && !explained[id] && s.FootprintMB > base+growth:
			// Heavy work the command text did not reveal (a wrapper script, a Makefile, a tool
			// AgentRM does not know): account for it and learn from it.
			l := &model.Lease{ID: newID(), Session: id, Agent: s.Agent, Kind: model.KHeavy,
				MemGB: (s.FootprintMB - base) / 1024, Purpose: "inferred: the process tree grew", Cwd: s.Cwd,
				TTL: m.cfg.BuildTTLMinutes, Created: now, Expires: now.Add(minutes(m.cfg.BuildTTLMinutes)),
				Verified: true, Evidence: fmt.Sprintf("process tree grew from %.1f to %.1f GB", base/1024, s.FootprintMB/1024),
				LastUsed: now, BaseFootprintMB: base, PeakMB: s.FootprintMB - base}
			if c := m.lastCmd(id); c != "" {
				l.Cmd = trunc(c, 200)
			}
			m.st.Leases[l.ID] = l
			m.event("HEAVY session %s (%s) grew by %.1f GB without a matching lease; recorded as lease %s%s",
				short(id), s.Agent, (s.FootprintMB-base)/1024, l.ID, cmdSuffix(l.Cmd))
		case heavy != nil:
			heavy.Expires = now.Add(minutes(heavy.TTL))
			heavy.PeakMB = max(heavy.PeakMB, s.FootprintMB-base)
		case heavy == nil && !explained[id]:
			// Track the idle baseline slowly, so a long-lived tool raises it gradually.
			m.footBase[id] = 0.9*base + 0.1*s.FootprintMB
		}
	}
	held := map[string]bool{}
	for _, l := range m.st.Leases {
		held[l.Session] = true
	}
	for id, s := range m.st.Sessions {
		if s.FootprintMB > m.cfg.UnleasedGB*1024 && !held[id] && now.Sub(s.FlaggedAt) > time.Hour {
			s.FlaggedAt = now
			m.event("UNLEASED-USE session %s (%s) is using %.1f GB in its process tree without any lease.",
				short(id), s.Agent, s.FootprintMB/1024)
		}
	}
}

// measureBuilds sets what a session's leases of one kind that hold no resource use. What a
// build uses is what it started: only processes started since the grant count, by footprint,
// so a simulator, a Gradle daemon or a dev server the session already had is not charged to it.
// A process started after a later lease of another kind is that lease's: each process is
// charged once. Leases of the same kind are measured as one, since an agent that asks for more
// gets a second lease for the difference and its build's processes cannot say which they belong
// to: what they use together fills the oldest lease's promise first, and the newest takes the
// rest. They are judged together too: a session is flagged for going well over what it asked
// for in all, not because its top-up took the excess. Must hold mu.
func (m *Manager) measureBuilds(ls []*model.Lease, tree []*sys.Proc, bound map[int]bool, now time.Time) {
	oldestFirst(ls)
	var wins [][2]time.Time // each lease's, from its grant to the session's next one
	for _, l := range ls {
		to := m.nextGrant(l)
		if !to.IsZero() {
			to = to.Add(-2 * time.Second)
		}
		wins = append(wins, [2]time.Time{l.Created.Add(-2 * time.Second), to})
	}
	left := m.usedIn(tree, wins, bound)
	usedMB, askedGB := 0.0, 0.0
	for i, l := range ls {
		l.InsideMB = m.daemonGrowthMB(l)
		own := left
		if i < len(ls)-1 {
			own = min(left, max(0, l.MemGB*1024-l.InsideMB))
		}
		left -= own
		l.UsedMB = own + l.InsideMB
		peak := own // growth of Docker's VM is not known to be the lease's (daemonGrowthMB)
		if daemonHost(l.Kind) != model.RKDockerVM {
			peak = l.UsedMB
		}
		l.PeakMB = max(l.PeakMB, peak)
		usedMB += peak
		askedGB += l.MemGB
	}
	m.checkOverrunOf(ls, usedMB, askedGB, now)
}

func cmdSuffix(c string) string {
	if c == "" {
		return ""
	}
	return " (last command: " + trunc(c, 80) + ")"
}

func (m *Manager) findLease(session, kind string) *model.Lease {
	for _, l := range m.st.Leases {
		if l.Session == session && l.Kind == kind {
			return l
		}
	}
	return nil
}

func (m *Manager) lastCmd(session string) string {
	if rec := m.recent[session]; len(rec) > 0 {
		return rec[len(rec)-1].Cmd
	}
	return ""
}

// inferred is a lease AgentRM recorded itself because a session's process tree grew.
func inferred(l *model.Lease) bool {
	return l.Kind == model.KHeavy && strings.HasPrefix(l.Purpose, "inferred:")
}

// daemonGrowthMB is the part of what a lease's host grew by since the lease was granted that is
// counted as the lease's. A Gradle build runs inside the Gradle and Kotlin daemons, which are
// not in the agent's process tree (launchd is their parent), so measuring only what the session
// started saw the small gradlew launcher and charged builds 20 times what they "used"; a daemon
// that did not exist at the grant was started by the build and counts in full. A Docker build
// runs in Docker's VM.
//
// A host cannot say whose work grew it, so its growth is handed out once: oldest lease first,
// none given more than it saw grow since its own grant or more than it was promised, and in all
// no more than the most any of them saw. A lease granted later does not change what an older one
// is given, and one released does not make another's jump. Growth is real memory either way (it
// is in what is available); what is handed out only decides whose promise it fills.
//
// What is left once every promise is full is someone's overrun. For a Gradle daemon it goes to
// the newest lease, when all the leases on the daemon are one session's (they are judged together:
// checkOverrun): a build that goes well over is caught. When two sessions share the daemon, whose
// it is cannot be told, and it is nobody's: one session's build never gets another flagged.
//
// For Docker, what a lease is given only ever makes less of the promise held back
// (materialisedMB): the VM grows for reasons that have nothing to do with the lease (someone
// else's pull, an unleased build, memory its containers let go of), so its growth never flags an
// overrun, distrusts a session, teaches an estimate or takes a lease up. A wrong reading can make
// headroom off by the promise at most.
func (m *Manager) daemonGrowthMB(l *model.Lease) float64 {
	host := daemonHost(l.Kind)
	if host == "" || len(l.Resources) > 0 {
		return 0
	}
	var ls []*model.Lease
	one := true // all the leases on the host are one session's
	for _, o := range m.st.Leases {
		if daemonHost(o.Kind) == host && len(o.Resources) == 0 {
			ls = append(ls, o)
			one = one && o.Session == l.Session
		}
	}
	oldestFirst(ls)
	pool := 0.0
	for _, o := range ls {
		pool = max(pool, m.hostGrowthMB(o))
	}
	given := 0.0
	for _, o := range ls {
		c := min(m.hostGrowthMB(o), pool, o.MemGB*1024)
		pool -= c
		if o == l {
			given = c
		}
	}
	if host != model.RKDockerVM && one && l == ls[len(ls)-1] {
		given += pool
	}
	return given
}

// hostGrowthMB is what a lease's host grew by since the lease was granted.
func (m *Manager) hostGrowthMB(l *model.Lease) float64 {
	b := l.DaemonBaseMB
	if b == nil {
		return 0
	}
	if daemonHost(l.Kind) == model.RKDockerVM {
		base, ok := b[dockerOwnKey]
		if !ok || b[dockerRebaseKey] == 1 || !m.dockerMeasured || m.dockerStale {
			return 0
		}
		// Beyond the most its containers held since the base: a VM that kept what stopped
		// containers held has not grown, and an unrelated stack's growth is its own.
		return max(0, m.dockerFullMB-max(b[dockerPeakKey], m.dockerContainersMB)-base)
	}
	grew := 0.0
	for id, r := range m.st.Resources {
		if r.Kind == daemonHost(l.Kind) && r.Unseen == nil {
			grew += max(0, r.MemMB-b[id])
		}
	}
	return grew
}

// inVM is a Docker lease whose work could only be seen as growth of Docker's VM: what it used
// is not known, so it teaches nothing and gets no receipt.
func inVM(l *model.Lease) bool {
	return daemonHost(l.Kind) == model.RKDockerVM && !l.HadResources
}

// oldestFirst orders leases by grant, two granted at the same moment by ID.
func oldestFirst(ls []*model.Lease) {
	sort.Slice(ls, func(i, j int) bool {
		if !ls[i].Created.Equal(ls[j].Created) {
			return ls[i].Created.Before(ls[j].Created)
		}
		return ls[i].ID < ls[j].ID
	})
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// heldGB is what a session holds of a kind of lease, in all.
func (m *Manager) heldGB(session, kind string) float64 {
	gb := 0.0
	for _, l := range m.st.Leases {
		if l.Session == session && l.Kind == kind {
			gb += l.MemGB
		}
	}
	return gb
}

// Keys of a Docker lease's DaemonBaseMB: what the VM held beyond its containers when the lease
// was granted (or first measured), the most its containers have held since, and a mark that
// the next measurement is a new base.
const (
	dockerOwnKey    = "docker-vm:own"
	dockerPeakKey   = "docker:containers-peak"
	dockerRebaseKey = "docker:rebase"
)

// dockerOwnMB is what Docker's VM holds beyond what its containers hold, as of the last sweep.
func (m *Manager) dockerOwnMB() float64 {
	return m.dockerFullMB - m.dockerContainersMB
}

// trackDockerBase keeps a Docker lease's base. While the VM is not measured (Docker stopped or
// restarting, the process table unread) the next measurement becomes the base: a restarted VM
// starts small, and nothing it did in between is known. A lease holding its containers is
// measured by them; its base follows the VM. The most the containers have held since the base
// is kept: the VM keeps what stopped containers held, and that is not growth (hostGrowthMB). A
// base is never lowered by a low reading: that would count the recovery as growth. Must hold mu.
func (m *Manager) trackDockerBase(l *model.Lease) {
	if daemonHost(l.Kind) != model.RKDockerVM {
		return
	}
	if !m.dockerMeasured {
		l.DaemonBaseMB = map[string]float64{dockerRebaseKey: 1}
		return
	}
	if _, ok := l.DaemonBaseMB[dockerOwnKey]; !ok || l.DaemonBaseMB[dockerRebaseKey] == 1 || len(l.Resources) > 0 {
		l.DaemonBaseMB = m.dockerBase()
		return
	}
	l.DaemonBaseMB[dockerPeakKey] = max(l.DaemonBaseMB[dockerPeakKey], m.dockerContainersMB)
}

// dockerBase is a new Docker lease base from the last sweep's measurement.
func (m *Manager) dockerBase() map[string]float64 {
	return map[string]float64{dockerOwnKey: m.dockerOwnMB(), dockerPeakKey: m.dockerContainersMB}
}

// startedMB is what shows something was started under a lease: its use, less growth of Docker's
// VM, which someone else's pull moves as well. A Docker lease is taken up when its session runs
// its command (the hook asks for the lease again) or its containers are bound to it.
func (m *Manager) startedMB(l *model.Lease) float64 {
	used := m.materialisedMB(l)
	if len(l.Resources) == 0 && daemonHost(l.Kind) == model.RKDockerVM {
		used = max(0, used-l.InsideMB)
	}
	return used
}

// daemonHost is the kind of resource a lease's work runs inside rather than in a process its
// session starts: Gradle's daemons, or Docker's VM for a Docker build (BuildKit runs in the VM)
// and for compose and clusters until their containers are bound to the lease. "" for the rest.
func daemonHost(kind string) string {
	switch kind {
	case model.KGradle:
		return model.RKGradle
	case model.KDockerBuild, model.KCompose, model.KCluster:
		return model.RKDockerVM
	}
	return ""
}
