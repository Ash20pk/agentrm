package manager

import (
	"fmt"
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
	// A session that already holds a lease of this kind reuses it.
	for _, l := range m.st.Leases {
		if l.Session == c.Session && l.Kind == r.Kind {
			l.Expires = now.Add(minutes(l.TTL))
			l.LastUsed = now
			return &api.AcquireResp{Granted: true, Lease: l, Reused: true}, nil, nil
		}
	}
	memGB := m.effectiveGB(r.Kind, cwd, r.MemGB, c.Session, now)
	fast := m.readFast() // pressure, load and disk right now, not as of the last sweep
	m.mem.PressureLevel, m.mem.Load1, m.mem.Cores, m.mem.DiskFreeGB = fast.PressureLevel, fast.Load1, fast.Cores, fast.DiskFreeGB
	if model.DiskKinds[r.Kind] && m.mem.DiskFreeGB >= 0 && m.mem.DiskFreeGB < m.cfg.MinDiskGB {
		return &api.AcquireResp{Reason: fmt.Sprintf(
			"the disk is nearly full (%.1f GB free on /, minimum %.0f GB). Prune Docker images or build caches first",
			m.mem.DiskFreeGB, m.cfg.MinDiskGB)}, nil, nil
	}
	if model.CPUKinds[r.Kind] && m.mem.Cores > 0 && m.mem.Load1 > float64(m.mem.Cores)*m.cfg.MaxLoadPerCore {
		return &api.AcquireResp{Reason: fmt.Sprintf("the CPU is saturated (load %.1f on %d cores, limit %.1f per core). %s",
			m.mem.Load1, m.mem.Cores, m.cfg.MaxLoadPerCore, m.holders())}, nil, nil
	}
	if ok, why, pos := m.admit(c.Session, r.Kind, memGB*1024, now); !ok {
		m.kickSweep() // sample fast while someone waits
		return &api.AcquireResp{Reason: why + ". " + m.holders(), Queue: pos}, nil, nil
	}
	ttl := r.TTLMinutes
	if ttl == 0 {
		ttl = m.cfg.DefaultTTLMinutes
		if model.BuildKinds[r.Kind] {
			ttl = m.cfg.BuildTTLMinutes
		}
	}
	l := &model.Lease{ID: newID(), Session: c.Session, Agent: c.Agent, Kind: r.Kind, MemGB: memGB,
		Purpose: r.Purpose, Cmd: trunc(r.Cmd, 200), Cwd: cwd, TTL: ttl, Created: now,
		Expires: now.Add(minutes(ttl)), Verified: true, Evidence: "requested before starting", LastUsed: now,
		Declared: r.MemGB > 0}
	if r.UntilMinutes > 0 {
		u := now.Add(minutes(r.UntilMinutes))
		l.Until = &u
	}
	if s := m.st.Sessions[c.Session]; s != nil {
		l.BaseFootprintMB = s.FootprintMB
	}
	m.st.Leases[l.ID] = l
	m.event("GRANT %s %s %.1f GB to %s (%s): %s", l.ID, l.Kind, memGB, c.Session, c.Agent, l.Purpose)
	m.kickSweep() // bind what it starts as soon as it appears
	return &api.AcquireResp{Granted: true, Lease: l}, nil, nil
}

func (m *Manager) dropLease(l *model.Lease, why string) {
	m.learn(l)
	delete(m.st.Questions, "idle:"+l.ID)
	for _, rid := range l.Resources {
		if r := m.st.Resources[rid]; r != nil && r.LeaseID == l.ID {
			r.LeaseID = ""
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
		switch {
		case now.After(l.Expires):
			m.dropLease(l, "expired (not renewed or released)")
			continue
		case !model.BuildKinds[l.Kind] && l.Kind != model.KHeavy && len(l.Resources) == 0 &&
			now.Sub(l.Created) > 15*time.Minute && m.materialisedMB(l) < 50:
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
	for apid, mb := range sys.SubtreeRSS(procs) {
		kind := sys.AgentKind(procs[apid])
		id := m.sessionOf(apid, kind)
		s := m.st.Sessions[id]
		if s == nil {
			s = &model.Session{ID: id, Agent: kind, LastSeen: now}
			m.st.Sessions[id] = s
		}
		s.FootprintMB, s.AgentPID = mb, apid
		live[id] = true
	}
	for id, s := range m.st.Sessions {
		if !live[id] {
			s.FootprintMB = 0
		}
	}
	explained := map[string]bool{} // sessions whose growth a lease accounts for
	for _, l := range m.st.Leases {
		if len(l.Resources) == 0 {
			if s := m.st.Sessions[l.Session]; s != nil {
				l.PeakMB = max(l.PeakMB, s.FootprintMB-l.BaseFootprintMB)
				m.checkOverrun(l, now)
			}
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
