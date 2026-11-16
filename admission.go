package main

// Admission: decide on what the machine measures, not on a ledger.
//
// The old rule compared a sum of lease estimates with "total memory minus a fixed reserve".
// That ledger knew nothing about memory used by anything it did not track (apps, the Docker
// VM's overhead, wrapper scripts), so it could report plenty of room on a machine deep in swap.
//
// The rule now:
//
//	headroom = memory the kernel can hand out right now
//	         - promises: the part of each granted lease that has not shown up as real use yet
//	         - a margin kept free
//
// and a request is refused, whatever the headroom, while the machine is thrashing (the kernel
// reports memory pressure, or it is swapping out fast). Real use by anyone, tracked or not,
// lowers "memory the kernel can hand out" by itself. Leases only have to cover the gap between
// a grant and the moment the memory is really taken.
//
// When a request does not fit, the manager does not just say no: it queues the request
// (first come, first served) and starts reclaiming from whoever is using their lease least.

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

type waiter struct {
	Session  string    `json:"session"`
	Kind     string    `json:"kind"`
	NeedMB   float64   `json:"need_mb"`
	Since    time.Time `json:"since"`
	LastSeen time.Time `json:"last_seen"`
}

const waiterTimeout = 90 * time.Second

// materialisedMB is how much of a lease's estimate is already real, measured use.
func (d *Daemon) materialisedMB(l *Lease) float64 {
	used := 0.0
	for _, rid := range l.Resources {
		if r := d.st.Resources[rid]; r != nil {
			used += r.MemMB
		}
	}
	if len(l.Resources) == 0 {
		// Builds and not-yet-visible resources: growth of the session's process tree since the grant.
		if s := d.st.Sessions[l.Session]; s != nil {
			used = max(0, s.FootprintMB-l.BaseFootprintMB)
		}
	}
	return used
}

// pendingMB is the memory promised to leases that they have not taken yet.
func (d *Daemon) pendingMB() float64 {
	total := 0.0
	for _, l := range d.st.Leases {
		total += max(0, l.MemGB*1024-d.materialisedMB(l))
	}
	return total
}

func (d *Daemon) headroomMB() float64 {
	return d.mem.AvailableMB - d.pendingMB() - d.cfg.HeadroomGB*1024
}

// thrashing reports whether the machine is short of memory right now, and why.
func (d *Daemon) thrashing() (bool, string) {
	if d.mem.PressureLevel >= 2 {
		lvl := "warning"
		if d.mem.PressureLevel >= 4 {
			lvl = "critical"
		}
		return true, "macOS reports memory pressure (" + lvl + ")"
	}
	if d.cfg.MaxSwapMBPerMin > 0 && d.swapRate > d.cfg.MaxSwapMBPerMin {
		return true, fmt.Sprintf("the Mac is swapping out %.0f MB a minute (limit %.0f)", d.swapRate, d.cfg.MaxSwapMBPerMin)
	}
	return false, ""
}

func (d *Daemon) liveWaiters(now time.Time) []*waiter {
	out := d.waiters[:0]
	for _, w := range d.waiters {
		if now.Sub(w.LastSeen) < waiterTimeout {
			out = append(out, w)
		}
	}
	d.waiters = out
	return out
}

func (d *Daemon) dropWaiter(session, kind string) {
	out := d.waiters[:0]
	for _, w := range d.waiters {
		if w.Session != session || w.Kind != kind {
			out = append(out, w)
		}
	}
	d.waiters = out
}

// admit decides whether needMB can be granted to session now. On refusal it queues the
// request and starts reclaiming; the reason says what was done and when to retry.
func (d *Daemon) admit(session, kind string, needMB float64, now time.Time) (bool, string) {
	// Can never fit, even on an empty machine: say so rather than queueing forever.
	if ceiling := d.mem.TotalMB - d.mem.WiredMB - d.cfg.HeadroomGB*1024; needMB > ceiling {
		return false, fmt.Sprintf("%.1f GB can never fit on this Mac (%.0f GB total, about %.1f GB usable at best)",
			needMB/1024, d.mem.TotalMB/1024, ceiling/1024)
	}
	waiters := d.liveWaiters(now)
	var me *waiter
	for _, w := range waiters {
		if w.Session == session && w.Kind == kind {
			me = w
		}
	}
	refuse := func(why string) (bool, string) {
		if me == nil {
			me = &waiter{Session: session, Kind: kind, Since: now}
			d.waiters = append(d.waiters, me)
		}
		me.NeedMB, me.LastSeen = needMB, now
		pos := 1
		for _, w := range d.waiters {
			if w.Session != session && w.Since.Before(me.Since) {
				pos++
			}
		}
		return false, fmt.Sprintf("%s. %s You are number %d in the queue; retry in a minute (your place is kept for %s between tries)",
			why, d.reclaim(needMB, now), pos, waiterTimeout)
	}

	if bad, why := d.thrashing(); bad {
		return refuse(why)
	}
	head := d.headroomMB()
	// First come, first served: do not take memory an earlier waiter is still waiting for.
	ahead := 0.0
	for _, w := range waiters {
		if w.Session != session && (me == nil || w.Since.Before(me.Since)) {
			ahead += w.NeedMB
		}
	}
	if needMB > head {
		return refuse(fmt.Sprintf("not enough memory: %.1f GB needed, %.1f GB can be handed out (%.1f GB available, %.1f GB promised to other leases, %.1f GB kept free)",
			needMB/1024, max(0, head)/1024, d.mem.AvailableMB/1024, d.pendingMB()/1024, d.cfg.HeadroomGB))
	}
	if needMB > head-ahead {
		return refuse(fmt.Sprintf("%.1f GB is free but earlier requests are waiting for %.1f GB of it", head/1024, ahead/1024))
	}
	d.dropWaiter(session, kind)
	return true, ""
}

// reclaim finds memory for a blocked request by pressing those who use theirs least:
// unclaimed resources first, then unverified claims, then the longest-idle leases. Their
// usual deadlines are shortened to the pressure grace period. It returns what it did.
func (d *Daemon) reclaim(needMB float64, now time.Time) string {
	grace := time.Duration(d.cfg.GraceMinutes) * time.Minute
	fast := time.Duration(d.cfg.PressureGraceMinutes) * time.Minute
	minIdle := time.Duration(d.cfg.PressureIdleMinutes) * time.Minute
	deadline := now.Add(fast)
	var done []string
	freed := 0.0

	// 1. Unclaimed resources: bring their stop forward.
	var orphans []*Resource
	for _, r := range d.st.Resources {
		if r.LeaseID == "" && !r.Pinned && !d.neverStop(r) && r.OrphanSince != nil {
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
		freed += r.MemMB
		done = append(done, fmt.Sprintf("unclaimed %s (%.1f GB)", r.ID, r.MemMB/1024))
	}

	// 2 and 3. Leases: unverified claims, then idle ones, longest idle first.
	type cand struct {
		l    *Lease
		mem  float64
		idle time.Duration
	}
	var cands []cand
	for _, l := range d.st.Leases {
		if len(l.Resources) == 0 {
			continue
		}
		idle := now.Sub(l.LastUsed)
		if !l.Verified || idle >= minIdle {
			cands = append(cands, cand{l, d.materialisedMB(l), idle})
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].l.Verified != cands[j].l.Verified {
			return !cands[i].l.Verified
		}
		return cands[i].idle > cands[j].idle
	})
	for _, c := range cands {
		if freed >= needMB {
			break
		}
		l := c.l
		// Ask now (or shorten an open question) so that no answer releases it at the deadline.
		asked := deadline.Add(-grace)
		if l.IdleAsked == nil || l.IdleAsked.After(asked) {
			l.IdleAsked = &asked
			d.askHolder(l, now, deadline, "Another agent is waiting for memory and")
		}
		freed += c.mem
		done = append(done, fmt.Sprintf("lease %s of %s (%.1f GB, unused %s)", l.ID, short(l.Session), c.mem/1024, c.idle.Round(time.Minute)))
	}

	switch {
	case len(done) == 0:
		return "Nothing is idle enough to reclaim: every lease has been used in the last " +
			fmt.Sprintf("%d min.", d.cfg.PressureIdleMinutes)
	case d.cfg.DryRun:
		return "Would reclaim " + strings.Join(done, ", ") + ", but agentrm is in dry run, so nothing will be stopped."
	}
	d.event("RECLAIM for a blocked %.1f GB request: %s; deadline %s", needMB/1024, strings.Join(done, ", "), deadline.Format("15:04"))
	return "Reclaiming " + strings.Join(done, ", ") + fmt.Sprintf(" by %s unless their holders answer.", deadline.Format("15:04"))
}

// askHolder puts the idle question to a lease's holder (and to you, for your own leases).
func (d *Daemon) askHolder(l *Lease, now, deadline time.Time, lead string) {
	what := strings.Join(l.Resources, ", ")
	d.st.Questions["idle:"+l.ID] = &Question{ResourceID: strings.Join(l.Resources, ","), Created: now, For: l.Session,
		Delivered: map[string]bool{}, Answers: map[string]string{},
		Text: fmt.Sprintf("[agentrm] %s your lease %s on %s has not been used for %s. Still need it? Run "+
			"agentrm claim %s --purpose \"<why>\" to keep it, or agentrm release %s. "+
			"No answer by %s releases it.", lead, l.ID, what, now.Sub(l.LastUsed).Round(time.Minute),
			l.Resources[0], l.ID, deadline.Format("15:04"))}
	if l.Session == "you" {
		d.event("ASKYOU %s, which you started, has not been used for %s. It is released at %s unless you claim it in AgentRM.",
			what, now.Sub(l.LastUsed).Round(time.Minute), deadline.Format("15:04"))
		notify("Still using "+what+"?", "Unused for "+now.Sub(l.LastUsed).Round(time.Minute).String()+
			". Released at "+deadline.Format("15:04")+" unless you claim it.")
		return
	}
	d.event("IDLE lease %s (%s) unused for %s: asking %s; deadline %s", l.ID, what,
		now.Sub(l.LastUsed).Round(time.Minute), short(l.Session), deadline.Format("15:04"))
}

// ---- per-session footprint: what each agent's process tree really uses ----

// subtreeRSS sums the resident memory of every descendant of each agent process.
func subtreeRSS(procs map[int]*proc) map[int]float64 {
	out := map[int]float64{}
	owner := map[int]int{} // pid -> agent pid (0 = none), memoised
	var find func(pid, depth int) int
	find = func(pid, depth int) int {
		if v, ok := owner[pid]; ok {
			return v
		}
		p := procs[pid]
		if p == nil || pid <= 1 || depth > 64 {
			return 0
		}
		a := 0
		if par := procs[p.ppid]; par != nil && agentKind(par) != "" {
			a = par.pid
		} else {
			a = find(p.ppid, depth+1)
		}
		owner[pid] = a
		return a
	}
	for pid, p := range procs {
		if agentKind(p) != "" {
			if _, ok := out[pid]; !ok {
				out[pid] = 0
			}
			continue
		}
		if a := find(pid, 0); a != 0 {
			out[a] += p.rssMB
		}
	}
	return out
}

func (d *Daemon) sessionOf(agentPID int, kind string) string {
	if kind == "claude" {
		if s := d.agentSessions[agentPID]; s != "" {
			return s
		}
		return fmt.Sprintf("claude-%d", agentPID)
	}
	return fmt.Sprintf("codex-%d", agentPID)
}

// updateFootprints records each live agent session's process-tree memory, feeds it into
// build leases (which hold no discoverable resource), and flags heavy use without a lease.
func (d *Daemon) updateFootprints(procs map[int]*proc, now time.Time) {
	live := map[string]bool{}
	for apid, mb := range subtreeRSS(procs) {
		id := d.sessionOf(apid, agentKind(procs[apid]))
		s := d.st.Sessions[id]
		if s == nil {
			s = &Session{ID: id, Agent: agentKind(procs[apid]), LastSeen: now}
			d.st.Sessions[id] = s
		}
		s.FootprintMB, s.AgentPID = mb, apid
		live[id] = true
	}
	for id, s := range d.st.Sessions {
		if !live[id] {
			s.FootprintMB = 0
		}
	}
	held := map[string]bool{}
	for _, l := range d.st.Leases {
		held[l.Session] = true
		if len(l.Resources) == 0 {
			if s := d.st.Sessions[l.Session]; s != nil {
				l.PeakMB = max(l.PeakMB, s.FootprintMB-l.BaseFootprintMB)
				d.checkOverrun(l, now)
			}
		}
	}
	for id, s := range d.st.Sessions {
		if s.FootprintMB > d.cfg.UnleasedGB*1024 && !held[id] && now.Sub(s.FlaggedAt) > time.Hour {
			s.FlaggedAt = now
			d.event("UNLEASED-USE session %s (%s) is using %.1f GB in its process tree without any lease. "+
				"It is counted against available memory; the command that started it was not recognised as heavy.",
				short(id), s.Agent, s.FootprintMB/1024)
		}
	}
}
