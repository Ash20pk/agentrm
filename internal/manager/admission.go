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
//
// A queued request is not polled for. The queue remembers what was asked, and when it fits
// the lease is granted there and then (grantWaiters) and its session is told: a client that
// stayed connected (acquire --wait) is answered, and the inbox carries it to the next tool
// call. A granted lease nobody takes up within grant_hold_minutes goes to the next in line.

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/discover"
	"agentrm/internal/model"
)

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
		// Builds and not-yet-visible resources: what the session started since the grant (an
		// inferred lease: the growth of its whole tree).
		if inferred(l) {
			if s := m.st.Sessions[l.Session]; s != nil {
				used = max(0, s.FootprintMB-l.BaseFootprintMB)
			}
		} else {
			used = l.UsedMB
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

// queueKeep is how long a queued request keeps its place after its session was last heard from.
func (m *Manager) queueKeep() time.Duration { return minutes(max(1, m.cfg.QueueMinutes)) }

// grantHold is how long a lease granted from the queue waits for its session to take it up.
func (m *Manager) grantHold() time.Duration { return minutes(max(1, m.cfg.GrantHoldMinutes)) }

// liveWaiters drops waiters whose session has gone quiet and returns the rest in queue order:
// higher priority first, then first come. A waiter is live while a client is connected and
// waiting for it, or while its session is still talking (any tool call counts, so an agent
// that got on with other work keeps its place). A session that ended lost it already.
func (m *Manager) liveWaiters(now time.Time) []*model.Waiter {
	keep := m.queueKeep()
	out := m.waiters[:0]
	var gone []*model.Waiter
	for _, w := range m.waiters {
		heard := w.LastSeen
		// Your own session is touched by everything you do in the app, so only the request counts.
		// A restore waits on its own retry only: the session getting on with other work does not
		// keep it (it was never granted from the queue, yet held up everyone behind it).
		if s := m.st.Sessions[w.Session]; s != nil && w.Session != "you" && !w.Retry && s.LastSeen.After(heard) {
			heard = s.LastSeen
		}
		if len(m.parked[w]) > 0 || now.Sub(heard) < keep {
			out = append(out, w)
		} else {
			gone = append(gone, w)
		}
	}
	for i := len(out); i < len(m.waiters); i++ {
		m.waiters[i] = nil
	}
	sort.SliceStable(out, func(i, j int) bool { return ahead(out[i], out[j]) })
	m.waiters = out
	for _, w := range gone {
		m.wake(w, nil)
	}
	return out
}

func ahead(a, b *model.Waiter) bool {
	if a.Priority != b.Priority {
		return a.Priority > b.Priority
	}
	return a.Since.Before(b.Since)
}

// dropWaiter takes a request out of the queue. A client still connected for it asks again.
func (m *Manager) dropWaiter(session, kind string) {
	out := m.waiters[:0]
	var dropped []*model.Waiter
	for _, w := range m.waiters {
		if w.Session != session || w.Kind != kind {
			out = append(out, w)
		} else {
			dropped = append(dropped, w)
		}
	}
	for i := len(out); i < len(m.waiters); i++ {
		m.waiters[i] = nil
	}
	m.waiters = out
	for _, w := range dropped {
		m.wake(w, nil)
	}
}

func (m *Manager) priority(session string) int {
	if s := m.st.Sessions[session]; s != nil {
		return s.Priority
	}
	return 0
}

// busy says why work of this kind cannot start now for a reason other than memory: a nearly
// full disk or a saturated CPU. Such a request is refused, not queued.
func (m *Manager) busy(kind string) string {
	if model.DiskKinds[kind] && m.mem.DiskFreeGB >= 0 && m.mem.DiskFreeGB < m.cfg.MinDiskGB {
		return fmt.Sprintf("the disk is nearly full (%.1f GB free on /, minimum %.0f GB). Prune Docker images or build caches first",
			m.mem.DiskFreeGB, m.cfg.MinDiskGB)
	}
	if model.CPUKinds[kind] && m.mem.Cores > 0 && m.mem.Load1 > float64(m.mem.Cores)*m.cfg.MaxLoadPerCore {
		return fmt.Sprintf("the CPU is saturated (load %.1f on %d cores, limit %.1f per core). %s",
			m.mem.Load1, m.mem.Cores, m.cfg.MaxLoadPerCore, m.holders())
	}
	return ""
}

// fits decides whether needMB can be granted to session now, given who else is waiting. me is
// the session's own place in the queue, if it has one. It changes nothing.
func (m *Manager) fits(session, kind, cwd string, needMB float64, me *model.Waiter, waiters []*model.Waiter) (ok bool, why string, backfill bool) {
	if bad, why := m.thrashing(); bad {
		return false, why, false
	}
	prio := m.priority(session)
	// Fair share: while someone else waits, one session may not grow past its share.
	others := 0
	for _, w := range waiters {
		if w.Session != session {
			others++
		}
	}
	if others > 0 && m.cfg.MaxSessionShare > 0 && prio == 0 {
		if held, share := m.heldMB(session), m.cfg.MaxSessionShare*m.usableMB(); held+needMB > share {
			return false, fmt.Sprintf("you already hold %.1f GB; with others waiting, one session may hold at most %.1f GB (%.0f%% of usable memory)",
				held/1024, share/1024, 100*m.cfg.MaxSessionShare), false
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
		return false, fmt.Sprintf("not enough memory: %.1f GB needed, %.1f GB can be handed out (%.1f GB available, %.1f GB promised to other leases, %.1f GB kept free)",
			needMB/1024, max(0, head)/1024, m.mem.AvailableMB/1024, m.pendingMB()/1024, m.cfg.HeadroomGB), false
	}
	if needMB > head-aheadMB {
		if m.canBackfill(session, kind, cwd, me, waiters) {
			return true, "", true
		}
		return false, fmt.Sprintf("%.1f GB is free but earlier requests are waiting for %.1f GB of it", head/1024, aheadMB/1024), false
	}
	return true, "", false
}

// admit decides whether what want asks for can be granted now. On refusal it queues the
// request, remembering what was asked so the lease can be granted when it fits (grantWaiters),
// and starts reclaiming; the reason says what was done and how the session hears about it.
func (m *Manager) admit(want model.Waiter, now time.Time) (bool, string, int) {
	if ceiling := m.usableMB(); want.NeedMB > ceiling {
		return false, fmt.Sprintf("%.1f GB can never fit on this Mac (%.0f GB total, about %.1f GB usable at best)",
			want.NeedMB/1024, m.mem.TotalMB/1024, ceiling/1024), 0
	}
	waiters := m.liveWaiters(now)
	var me *model.Waiter
	for _, w := range waiters {
		if w.Session == want.Session && w.Kind == want.Kind {
			me = w
		}
	}
	ok, why, backfill := m.fits(want.Session, want.Kind, want.Cwd, want.NeedMB, me, waiters)
	if ok {
		m.dropWaiter(want.Session, want.Kind)
		if backfill {
			m.noteBackfill(want.Session, waiters, now)
			m.lastBackfill = true
		}
		return true, "", 0
	}
	if me == nil {
		me = &model.Waiter{Since: now}
		m.waiters = append(m.waiters, me)
	}
	since := me.Since
	*me = want
	me.Since, me.LastSeen, me.Pressed, me.Priority = since, now, now, m.priority(want.Session)
	pos := 1
	for _, w := range m.waiters {
		if w != me && ahead(w, me) {
			pos++
		}
	}
	return false, fmt.Sprintf("%s. %s You are number %d in the queue. %s",
		why, m.reclaim(want.NeedMB, want.Session, now), pos, m.howToWait(me)), pos
}

// howToWait tells a queued session how it gets its lease. Only Claude Code's hooks carry a
// message to the agent by themselves; anything else has to stay connected to hear.
func (m *Manager) howToWait(w *model.Waiter) string {
	if w.Retry {
		return fmt.Sprintf("Try again in a minute; your place is kept for %d min", int(m.queueKeep().Minutes()))
	}
	cmd := fmt.Sprintf("agentrm acquire --kind %s --purpose \"<why>\" --wait", w.Kind)
	if w.Via == "hook" {
		return fmt.Sprintf("Do not retry in a loop: the lease is granted to you as soon as it fits, and you are told on your next tool call; "+
			"you then have %d min to run the command again. If you have nothing else to do, run `%s` in the background: it returns when the lease is yours",
			int(m.grantHold().Minutes()), cmd)
	}
	return fmt.Sprintf("Your place is kept for %d min. To wait for it, run `%s`: it returns when the lease is yours",
		int(m.queueKeep().Minutes()), cmd)
}

// grantWaiters gives each queued request its lease as soon as it fits, in queue order, and
// tells its session: a connected client is answered at once, and the session's inbox gets the
// news for its next tool call. With press, holders are pressed again for those still waiting.
// Must hold mu.
func (m *Manager) grantWaiters(now time.Time, press bool) {
	for _, w := range append([]*model.Waiter(nil), m.liveWaiters(now)...) {
		if m.findLease(w.Session, w.Kind) != nil {
			m.dropWaiter(w.Session, w.Kind) // it got one some other way
			continue
		}
		if w.Retry || m.busy(w.Kind) != "" {
			continue
		}
		ok, _, backfill := m.fits(w.Session, w.Kind, w.Cwd, w.NeedMB, w, m.waiters)
		if backfill {
			m.noteBackfill(w.Session, m.waiters, now)
			m.lastBackfill = true
		}
		if !ok {
			if press && now.Sub(w.Pressed) >= time.Minute {
				w.Pressed = now
				m.reclaim(w.NeedMB, w.Session, now)
			}
			continue
		}
		l := m.grant(w, now, fmt.Sprintf(" (from the queue, after %s)", now.Sub(w.Since).Round(time.Second)))
		t := now
		l.Offered = &t
		by := now.Add(m.grantHold())
		m.st.Questions["grant:"+l.ID] = &model.Question{Created: now, For: w.Session,
			Delivered: map[string]bool{}, Answers: map[string]string{},
			Text: fmt.Sprintf("[agentrm] Your queued %s request was granted: lease %s (%.1f GB) is yours. "+
				"Run what you were waiting to run by %s; after that it goes to the next in the queue. "+
				"If you no longer need it, run: agentrm release %s", l.Kind, l.ID, l.MemGB, by.Format("15:04"), l.ID)}
		cp := *l
		parked := m.parked[w]
		m.removeWaiter(w)
		for _, ch := range parked {
			ch <- &api.AcquireResp{Granted: true, Lease: &cp}
		}
		delete(m.parked, w)
	}
}

// removeWaiter takes exactly w out of the queue without waking anyone.
func (m *Manager) removeWaiter(w *model.Waiter) {
	out := m.waiters[:0]
	for _, x := range m.waiters {
		if x != w {
			out = append(out, x)
		}
	}
	for i := len(out); i < len(m.waiters); i++ {
		m.waiters[i] = nil
	}
	m.waiters = out
}

// takeUp records that a lease granted from the queue reached its session.
func (m *Manager) takeUp(l *model.Lease) {
	if l != nil && l.Offered != nil {
		l.Offered = nil
		delete(m.st.Questions, "grant:"+l.ID)
	}
}

// ---- clients that stay connected until their lease is granted ----

// maxWait bounds how long one request may stay connected.
const maxWait = 30 * time.Minute

// park registers a connected client for the caller's queued request. Must hold mu.
func (m *Manager) park(session, kind string) chan *api.AcquireResp {
	for _, w := range m.waiters {
		if w.Session == session && w.Kind == kind {
			ch := make(chan *api.AcquireResp, 1)
			m.parked[w] = append(m.parked[w], ch)
			return ch
		}
	}
	return nil
}

// wake answers the clients connected for w and forgets them. A nil answer makes them ask
// again. Must hold mu.
func (m *Manager) wake(w *model.Waiter, resp *api.AcquireResp) {
	for _, ch := range m.parked[w] {
		ch <- resp
	}
	delete(m.parked, w)
}

func (m *Manager) unpark(ch chan *api.AcquireResp) {
	for w, chans := range m.parked {
		for i, c := range chans {
			if c == ch {
				m.parked[w] = append(chans[:i:i], chans[i+1:]...)
				if len(m.parked[w]) == 0 {
					delete(m.parked, w)
				}
				return
			}
		}
	}
}

// awaitGrant blocks one connected client until its queued request is granted, the client goes
// away, or its time is up. It holds no lock while it waits: a parked request costs one idle
// goroutine. Must be called without mu.
func (m *Manager) awaitGrant(ctx context.Context, ch chan *api.AcquireResp, c api.Caller, r api.Req) (any, error) {
	t := time.NewTimer(min(time.Duration(r.WaitSeconds)*time.Second, maxWait))
	defer t.Stop()
	var got *api.AcquireResp
	answered := false
	select {
	case got = <-ch:
		answered = true
	case <-ctx.Done():
	case <-t.C:
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unpark(ch)
	if !answered {
		select {
		case got = <-ch: // granted while this was on its way out
			answered = true
		default:
		}
	}
	if ctx.Err() != nil {
		// Nobody is listening. A lease granted a moment ago stays offered, and goes to the
		// next in the queue if the session does not take it up.
		return nil, ctx.Err()
	}
	if got != nil {
		m.takeUp(m.st.Leases[got.Lease.ID])
		m.dirty = true
		return snapshot(got), nil
	}
	// Time is up, or the request left the queue some other way: answer as of now.
	r.WaitSeconds = 0
	out, _, err := m.acquire(c, r)
	m.dirty = true
	if out != nil {
		out = snapshot(out).(*api.AcquireResp) // encoded after the deferred unlock
	}
	return out, err
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
		// Only what AgentRM will really stop frees memory: not pinned, not exempt, not a VM, and
		// not something last seen on a source that no longer answers (it cannot be stopped, and
		// counting on it would leave the request waiting on memory that never comes).
		if r.LeaseID == "" && r.Unseen == nil && !r.Pinned && !m.neverStop(r) && r.OrphanSince != nil && discover.CanAutoStop(r) && r.StartedBy != "you" {
			orphans = append(orphans, r)
		}
	}
	sort.Slice(orphans, func(i, j int) bool { return orphans[i].MemMB > orphans[j].MemMB })
	for _, r := range orphans {
		if freed >= needMB {
			break
		}
		// Dry run decides nothing: it neither shortens clocks nor marks anything for a direct
		// stop, which would otherwise take effect the moment dry run is turned off.
		if !m.dryFor(r) {
			if early := deadline.Add(-grace); r.OrphanSince.After(early) {
				r.OrphanSince = &early
			}
			r.Urgent = true
		}
		back := r.MemMB * m.returnRatio(r.Source) // what macOS really gets back
		freed += back
		done = append(done, fmt.Sprintf("unclaimed %s (%.1f GB, ~%.1f GB back)", r.ID, r.MemMB/1024, back/1024))
	}

	type cand struct {
		l    *model.Lease
		mem  float64
		idle time.Duration
		over bool
		prio int
		rank int // the holder's offer: 0 none, 1 low, 2 no answer, 3 high
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
		// Every holder that could give way is asked what it would cost (cooperative preemption).
		m.askYield(l, needMB, now, deadline)
		idle := now.Sub(l.LastUsed)
		over := m.cfg.MaxSessionShare > 0 && m.heldMB(l.Session) > share
		rank := m.costRank(l)
		if rank == 1 || !l.Verified || idle >= minIdle || over && idle >= minIdle/2 {
			// Releasing a lease frees its whole promise (what it has not taken yet stops being
			// held back) as well as what it uses.
			cands = append(cands, cand{l, max(l.MemGB*1024, m.materialisedMB(l)), idle, over, p, rank})
		}
	}
	sortCands(cands, func(c cand) int { return c.rank }, func(a, b cand) bool {
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
		freed += c.mem
		if c.rank == 1 && !m.cfg.DryRun {
			// Its holder said stopping it costs little: take it now, not at a deadline.
			done = append(done, fmt.Sprintf("lease %s of %s (%.1f GB, offered at low cost)", l.ID, short(l.Session), c.mem/1024))
			for _, rid := range l.Resources {
				if res := m.st.Resources[rid]; res != nil {
					t := now
					res.Owner, res.OwnerGoneAt, res.Urgent = l.Session, &t, true
				}
			}
			m.dropLease(l, "its holder offered it at low cost to a waiting request")
			continue
		}
		if !m.cfg.DryRun {
			// Pressed: its idle deadline moves up to the pressure grace period.
			asked := deadline.Add(-grace)
			if l.IdleAsked == nil || l.IdleAsked.After(asked) {
				l.IdleAsked = &asked
			}
		}
		why := fmt.Sprintf("unused %s", c.idle.Round(time.Minute))
		if c.over {
			why = "over its fair share, " + why
		}
		if c.rank == 0 {
			why = "offered by its holder"
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
			// The typical peak plus its spread, the spread capped at half the peak: one odd
			// run (a clean build, a build measured with a simulator in its tree) must not
			// double what every later build is charged. Real use is measured as it runs, and a
			// build that goes over is caught by the pressure checks.
			return max(max(mb+min(m.st.LearnedDev[key], mb/2), mb*1.15)/1024, 0.25)
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
