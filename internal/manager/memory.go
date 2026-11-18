package manager

// All of AgentRM's memory arithmetic, in one place. Every figure it grants, refuses, reclaims
// or charges by comes from these functions, and each one is checked against what really
// happened (see accuracy.go). Four questions, one definition each:
//
//   - how much is available: m.mem.AvailableMB (sys: what can be had without swapping), less
//     what is promised and the reserve: headroomMB;
//   - how much a process or a build holds: procMB (footprint), usedSince, materialisedMB;
//   - how much a request will need: estimateGB, effectiveGB;
//   - how much stopping something gives back: returnRatio (returns.go).
//
// Nothing outside this file should add up memory on its own.

import (
	"strings"
	"time"

	"agentrm/internal/model"
	"agentrm/internal/sys"
)

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

// procMB is what a process holds: its footprint where that can be read (what Activity Monitor
// shows: shared pages counted once, compressed and swapped pages included), else its resident
// size.
func (m *Manager) procMB(p *sys.Proc) float64 {
	if m.footprint != nil {
		if fp, ok := m.footprint(p.PID); ok {
			return fp
		}
	}
	return p.RSSMB
}

// usedSince is what the processes of a tree started at or after t hold now.
func (m *Manager) usedSince(tree []*sys.Proc, t time.Time) float64 {
	total := 0.0
	for _, p := range tree {
		if !p.Start.IsZero() && !p.Start.Before(t) {
			total += m.procMB(p)
		}
	}
	return total
}
