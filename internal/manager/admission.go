package manager

// Admission: decide on what the machine measures, not on a ledger.
//
//	can grant = memory the kernel can hand out now
//	          - promises (the part of each granted lease not yet taken as real use)
//	          - a margin kept free
//
// Nothing is granted while the Mac is thrashing. A request that does not fit is queued and
// reclamation starts. The queue is ordered by priority, then arrival; and while others wait,
// no session may hold more than its fair share.

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"agentrm/internal/discover"
	"agentrm/internal/model"
)

const waiterTimeout = 90 * time.Second

// minLearnMB is the smallest peak worth learning from.
const minLearnMB = 100

// materialisedMB is how much of a lease's estimate is already real, measured use.
func (m *Manager) materialisedMB(l *model.Lease) float64 {
	used := 0.0
	for _, rid := range l.Resources {
		if r := m.st.Resources[rid]; r != nil {
			used += r.MemMB
		}
	}
	if len(l.Resources) == 0 {
		// Builds and not-yet-visible resources: growth of the session's process tree since the grant.
		if s := m.st.Sessions[l.Session]; s != nil {
			used = max(0, s.FootprintMB-l.BaseFootprintMB)
		}
	}
	return used
}

// pendingMB is the memory promised to leases that they have not taken yet.
func (m *Manager) pendingMB() float64 {
	total := 0.0
	for _, l := range m.st.Leases {
		total += max(0, l.MemGB*1024-m.materialisedMB(l))
	}
	return total
}

func (m *Manager) headroomMB() float64 {
	return m.mem.AvailableMB - m.pendingMB() - m.cfg.HeadroomGB*1024
}

func (m *Manager) usableMB() float64 {
	return m.mem.TotalMB - m.mem.WiredMB - m.cfg.HeadroomGB*1024
}

// heldMB is what a session holds: for each lease, the larger of its promise and its real use.
func (m *Manager) heldMB(session string) float64 {
	total := 0.0
	for _, l := range m.st.Leases {
		if l.Session == session {
			total += max(l.MemGB*1024, m.materialisedMB(l))
		}
	}
	return total
}

// thrashing reports whether the machine is short of memory right now, and why.
//
// macOS sits at "warning" for hours while memory compression copes, with nothing being swapped
// out; refusing everything then would stop all work for no gain. So warning alone is not
// thrashing. It is: critical pressure; swapping out faster than the limit; or warning while
// swapping out at a third of it.
func (m *Manager) thrashing() (bool, string) {
	limit := m.cfg.MaxSwapMBPerMin
	switch {
	case m.mem.PressureLevel >= 4:
		return true, "macOS reports critical memory pressure"
	case limit > 0 && m.swapRate > limit:
		return true, fmt.Sprintf("the Mac is swapping out %.0f MB a minute (limit %.0f)", m.swapRate, limit)
	case limit > 0 && m.mem.PressureLevel >= 2 && m.swapRate > limit/3:
		return true, fmt.Sprintf("macOS reports memory pressure and is swapping out %.0f MB a minute", m.swapRate)
	}
	return false, ""
}

// liveWaiters drops waiters that stopped retrying and returns the rest in queue order:
// higher priority first, then first come.
func (m *Manager) liveWaiters(now time.Time) []*model.Waiter {
	out := m.waiters[:0]
	for _, w := range m.waiters {
		if now.Sub(w.LastSeen) < waiterTimeout {
			out = append(out, w)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return ahead(out[i], out[j]) })
	m.waiters = out
	return out
}

func ahead(a, b *model.Waiter) bool {
	if a.Priority != b.Priority {
		return a.Priority > b.Priority
	}
	return a.Since.Before(b.Since)
}

func (m *Manager) dropWaiter(session, kind string) {
	out := m.waiters[:0]
	for _, w := range m.waiters {
		if w.Session != session || w.Kind != kind {
			out = append(out, w)
		}
	}
	m.waiters = out
}

func (m *Manager) priority(session string) int {
	if s := m.st.Sessions[session]; s != nil {
		return s.Priority
	}
	return 0
}

// admit decides whether needMB can be granted to session now. On refusal it queues the
// request and starts reclaiming; the reason says what was done and when to retry.
func (m *Manager) admit(session, kind string, needMB float64, now time.Time) (bool, string, int) {
	if ceiling := m.usableMB(); needMB > ceiling {
		return false, fmt.Sprintf("%.1f GB can never fit on this Mac (%.0f GB total, about %.1f GB usable at best)",
			needMB/1024, m.mem.TotalMB/1024, ceiling/1024), 0
	}
	waiters := m.liveWaiters(now)
	var me *model.Waiter
	for _, w := range waiters {
		if w.Session == session && w.Kind == kind {
			me = w
		}
	}
	prio := m.priority(session)
	refuse := func(why string) (bool, string, int) {
		if me == nil {
			me = &model.Waiter{Session: session, Kind: kind, Since: now}
			m.waiters = append(m.waiters, me)
		}
		me.NeedMB, me.LastSeen, me.Priority = needMB, now, prio
		pos := 1
		for _, w := range m.waiters {
			if w != me && ahead(w, me) {
				pos++
			}
		}
		return false, fmt.Sprintf("%s. %s You are number %d in the queue; retry in a minute (your place is kept for %s between tries)",
			why, m.reclaim(needMB, session, now), pos, waiterTimeout), pos
	}

	if bad, why := m.thrashing(); bad {
		return refuse(why)
	}
	// Fair share: while someone else waits, one session may not grow past its share.
	others := 0
	for _, w := range waiters {
		if w.Session != session {
			others++
		}
	}
	if others > 0 && m.cfg.MaxSessionShare > 0 && prio == 0 {
		if held, share := m.heldMB(session), m.cfg.MaxSessionShare*m.usableMB(); held+needMB > share {
			return refuse(fmt.Sprintf("you already hold %.1f GB; with others waiting, one session may hold at most %.1f GB (%.0f%% of usable memory)",
				held/1024, share/1024, 100*m.cfg.MaxSessionShare))
		}
	}
	head := m.headroomMB()
	// Do not take memory a request ahead of this one is still waiting for.
	aheadMB := 0.0
	for _, w := range waiters {
		if w.Session != session && (me == nil || ahead(w, me)) && !(prio > w.Priority) {
			aheadMB += w.NeedMB
		}
	}
	if needMB > head {
		return refuse(fmt.Sprintf("not enough memory: %.1f GB needed, %.1f GB can be handed out (%.1f GB available, %.1f GB promised to other leases, %.1f GB kept free)",
			needMB/1024, max(0, head)/1024, m.mem.AvailableMB/1024, m.pendingMB()/1024, m.cfg.HeadroomGB))
	}
	if needMB > head-aheadMB {
		return refuse(fmt.Sprintf("%.1f GB is free but earlier requests are waiting for %.1f GB of it", head/1024, aheadMB/1024))
	}
	m.dropWaiter(session, kind)
	return true, "", 0
}

// reclaim finds memory for a blocked request by pressing those who use theirs least: unclaimed
// resources first, then sessions over their fair share, unverified claims, then the
// longest-idle leases. Urgent sessions are pressed last, and never for a normal request. Their
// deadlines shrink to the pressure grace period. It returns what it did.
func (m *Manager) reclaim(needMB float64, forSession string, now time.Time) string {
	grace := minutes(m.cfg.GraceMinutes)
	deadline := now.Add(minutes(m.cfg.PressureGraceMinutes))
	minIdle := minutes(m.cfg.PressureIdleMinutes)
	reqPrio := m.priority(forSession)
	var done []string
	freed := 0.0

	var orphans []*model.Resource
	for _, r := range m.st.Resources {
		// Only what AgentRM will really stop frees memory: not pinned, not exempt, not a VM.
		if r.LeaseID == "" && !r.Pinned && !m.neverStop(r) && r.OrphanSince != nil && discover.CanAutoStop(r) {
			orphans = append(orphans, r)
		}
	}
	sort.Slice(orphans, func(i, j int) bool { return orphans[i].MemMB > orphans[j].MemMB })
	for _, r := range orphans {
		if freed >= needMB {
			break
		}
		if early := deadline.Add(-grace); r.OrphanSince.After(early) {
			r.OrphanSince = &early
		}
		r.Urgent = true
		freed += r.MemMB
		done = append(done, fmt.Sprintf("unclaimed %s (%.1f GB)", r.ID, r.MemMB/1024))
	}

	type cand struct {
		l    *model.Lease
		mem  float64
		idle time.Duration
		over bool
		prio int
	}
	share := m.cfg.MaxSessionShare * m.usableMB()
	var cands []cand
	for _, l := range m.st.Leases {
		if len(l.Resources) == 0 || l.Session == forSession {
			continue
		}
		p := m.priority(l.Session)
		if p > reqPrio {
			continue // an urgent session is never pressed for a normal request
		}
		idle := now.Sub(l.LastUsed)
		over := m.cfg.MaxSessionShare > 0 && m.heldMB(l.Session) > share
		if !l.Verified || idle >= minIdle || over && idle >= minIdle/2 {
			cands = append(cands, cand{l, m.materialisedMB(l), idle, over, p})
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		switch {
		case a.prio != b.prio:
			return a.prio < b.prio
		case a.over != b.over:
			return a.over
		case a.l.Verified != b.l.Verified:
			return !a.l.Verified
		}
		return a.idle > b.idle
	})
	for _, c := range cands {
		if freed >= needMB {
			break
		}
		l := c.l
		asked := deadline.Add(-grace)
		if l.IdleAsked == nil || l.IdleAsked.After(asked) {
			l.IdleAsked = &asked
			m.askHolder(l, now, deadline, "Another agent is waiting for memory and")
		}
		freed += c.mem
		why := fmt.Sprintf("unused %s", c.idle.Round(time.Minute))
		if c.over {
			why = "over its fair share, " + why
		}
		done = append(done, fmt.Sprintf("lease %s of %s (%.1f GB, %s)", l.ID, short(l.Session), c.mem/1024, why))
	}

	switch {
	case len(done) == 0:
		return fmt.Sprintf("Nothing is idle enough to reclaim: every lease has been used in the last %d min.", m.cfg.PressureIdleMinutes)
	case m.cfg.DryRun:
		m.decide("reclaim", nil, fmt.Sprintf("would reclaim %s for a blocked %.1f GB request", strings.Join(done, ", "), needMB/1024))
		return "Would reclaim " + strings.Join(done, ", ") + ", but AgentRM is in dry run, so nothing will be stopped."
	}
	m.event("RECLAIM for a blocked %.1f GB request: %s; deadline %s", needMB/1024, strings.Join(done, ", "), deadline.Format("15:04"))
	return "Reclaiming " + strings.Join(done, ", ") + fmt.Sprintf(" by %s unless their holders answer.", deadline.Format("15:04"))
}

// ---- estimates ----

// projectOf is the estimate key's project part: the working directory's base name.
func projectOf(cwd string) string {
	if cwd == "" || cwd == "/" {
		return ""
	}
	return filepath.Base(cwd)
}

// estimateGB is the memory a new lease is expected to need: what past leases of this kind in
// this project really used, else of this kind anywhere (mean plus twice the typical deviation,
// and at least 15% over the mean), else the configured default.
func (m *Manager) estimateGB(kind, cwd string) float64 {
	for _, key := range []string{kind + "@" + projectOf(cwd), kind} {
		if strings.HasSuffix(key, "@") {
			continue
		}
		if mb := m.st.Learned[key]; mb > 0 {
			return max(max(mb+2*m.st.LearnedDev[key], mb*1.15)/1024, 0.25)
		}
	}
	if gb := m.cfg.KindMemGB[kind]; gb > 0 {
		return gb
	}
	return 1
}

// effectiveGB is the memory a request is charged. A requester may declare its own figure, but
// never below what this kind really used, and a session caught using far more than it declared
// loses the right to declare for an hour.
func (m *Manager) effectiveGB(kind, cwd string, declared float64, session string, now time.Time) float64 {
	est := m.estimateGB(kind, cwd)
	if declared <= 0 {
		return est
	}
	if s := m.st.Sessions[session]; s != nil && now.Before(s.DistrustUntil) {
		return max(declared, est)
	}
	for _, key := range []string{kind + "@" + projectOf(cwd), kind} {
		if mean := m.st.Learned[key]; mean > 0 {
			return max(declared, mean/1024)
		}
	}
	return declared
}

// checkOverrun flags a lease that took far more than its holder declared.
func (m *Manager) checkOverrun(l *model.Lease, now time.Time) {
	if !l.Declared || l.Overran || l.PeakMB <= 1.5*l.MemGB*1024+256 {
		return
	}
	l.Overran = true
	if s := m.st.Sessions[l.Session]; s != nil {
		s.DistrustUntil = now.Add(time.Hour)
	}
	m.event("OVERRAN lease %s (%s) declared %.1f GB and used %.1f GB. Memory figures from %s are ignored until %s.",
		l.ID, l.Kind, l.MemGB, l.PeakMB/1024, short(l.Session), now.Add(time.Hour).Format("15:04"))
}

// learn folds a finished lease's peak into the estimates for its kind and its project.
func (m *Manager) learn(l *model.Lease) {
	kind := l.Kind
	if rk, ok := strings.CutPrefix(kind, "claim:"); ok {
		kind = model.ResourceToLease[rk]
	}
	// Only leases that really ran something teach anything.
	if kind == "" || l.PeakMB < minLearnMB || (model.LeaseToResource[kind] != "" && !l.HadResources) {
		return
	}
	keys := []string{kind}
	if p := projectOf(l.Cwd); p != "" {
		keys = append(keys, kind+"@"+p)
	}
	for _, key := range keys {
		if old := m.st.Learned[key]; old > 0 {
			dev := l.PeakMB - old
			if dev < 0 {
				dev = -dev
			}
			m.st.LearnedDev[key] = 0.7*m.st.LearnedDev[key] + 0.3*dev
			m.st.Learned[key] = 0.7*old + 0.3*l.PeakMB
		} else {
			m.st.Learned[key] = l.PeakMB
		}
	}
}
