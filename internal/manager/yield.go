package manager

// Cooperative preemption. When a request is blocked, AgentRM does not only pick victims by
// rule: it asks each holder what giving up its lease would cost it right now. An agent between
// test runs can say "nothing", one in the middle of a migration "a lot". The cheapest offers are
// taken first, holders that do not answer fall back to the usual order (idle longest first),
// and an agent is never pressed harder for answering honestly.
//
// Answers are checked afterwards, against what the machine shows: a holder that said "none"
// and then used the thing again within half an hour misreported; a restart that took far
// longer than the holder said did too. Each session's later answers are weighted by its record
// (Session.Honesty), and a session with a poor record loses the right to declare memory too.

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/evidence"
	"agentrm/internal/model"
)

const (
	// yieldCheck is how long after an offer AgentRM watches whether it was true.
	yieldCheck = 30 * time.Minute
	maxYields  = 200
)

// costRank orders offers: lower is taken first. An offer from a session that has misreported
// (one wrong answer is enough, until honest ones outweigh it) counts as no answer.
func (m *Manager) costRank(l *model.Lease) int {
	y := m.offerFor(l.ID)
	if y == nil {
		return 2
	}
	if s := m.st.Sessions[l.Session]; s != nil && s.Honesty() < 0.6 {
		return 2
	}
	switch y.Cost {
	case "none":
		return 0
	case "low":
		return 1
	case "high":
		return 3
	}
	return 2
}

// offerFor is the current offer on a lease, if any.
func (m *Manager) offerFor(lease string) *model.Yield {
	for i := len(m.st.Yields) - 1; i >= 0; i-- {
		if y := m.st.Yields[i]; y.Lease == lease && y.Taken == nil {
			return y
		}
	}
	return nil
}

// askYield asks a holder what giving up its lease would cost, for a request waiting for needMB.
func (m *Manager) askYield(l *model.Lease, needMB float64, now, deadline time.Time) {
	key := "idle:" + l.ID
	if q := m.st.Questions[key]; q != nil && strings.Contains(q.Text, "agentrm yield") {
		return // asked already in this episode
	}
	what := strings.Join(l.Resources, ", ")
	if what == "" {
		what = l.Kind
	}
	m.st.Questions[key] = &model.Question{ResourceID: strings.Join(l.Resources, ","), Created: now, For: l.Session,
		Delivered: map[string]bool{}, Answers: map[string]string{},
		Text: fmt.Sprintf("[agentrm] Another agent is waiting for %.1f GB. What would giving up your lease %s on %s cost you right now? "+
			"Answer honestly, with one of: `agentrm yield %s --cost none` (you are done with it: it is released now); "+
			"`agentrm yield %s --cost low --restart-seconds N` (fine to stop; you can bring it back in about N seconds); "+
			"`agentrm yield %s --cost high` (you are in the middle of using it). Cheaper offers are taken first; AgentRM checks "+
			"afterwards whether each answer was true, and weighs your later answers by it. No answer by %s: it may be reclaimed if it is idle.",
			needMB/1024, l.ID, what, l.ID, l.ID, l.ID, deadline.Format("15:04"))}
	m.event("ASKYIELD lease %s (%s) of %s: what would giving it up cost?", l.ID, what, short(l.Session))
}

// yield is a holder's answer.
func (m *Manager) yield(c api.Caller, r api.Req) (any, []Effect, error) {
	l := m.st.Leases[r.LeaseID]
	if l == nil {
		return nil, nil, fmt.Errorf("no lease %s", r.LeaseID)
	}
	if l.Session != c.Session {
		return nil, nil, fmt.Errorf("lease %s is not yours", l.ID)
	}
	switch r.Cost {
	case "none", "low", "high":
	default:
		return nil, nil, fmt.Errorf("cost must be none, low or high")
	}
	now := m.now()
	y := &model.Yield{ID: newID(), Lease: l.ID, Session: c.Session, Kind: l.Kind, Resources: append([]string(nil), l.Resources...),
		Cost: r.Cost, RestartSeconds: r.RestartSeconds, At: now}
	if old := m.offerFor(l.ID); old != nil {
		old.Taken, old.Verdict = &now, "superseded"
	}
	m.st.Yields = append(m.st.Yields, y)
	if len(m.st.Yields) > maxYields {
		m.st.Yields = m.st.Yields[len(m.st.Yields)-maxYields:]
	}
	delete(m.st.Questions, "idle:"+l.ID)
	note := ""
	switch r.Cost {
	case "none":
		// Done with it: release now, and what it holds goes on the owner-gone path, at once if
		// someone is waiting.
		blocked := len(m.liveWaiters(now)) > 0
		for _, rid := range l.Resources {
			if res := m.st.Resources[rid]; res != nil {
				t := now
				res.Owner, res.OwnerGoneAt, res.Urgent = c.Session, &t, blocked
			}
		}
		y.Taken = &now
		m.dropLease(l, "its holder said it is done with it")
		m.grantWaiters(now, false)
		m.kickSweep()
		note = "released; thank you"
	case "low":
		note = "noted: it will be taken before anything whose holder did not answer"
	case "high":
		note = "noted: it will be taken last"
	}
	m.event("YIELD lease %s of %s: cost %s, restart %ds (%s)", l.ID, short(c.Session), r.Cost, r.RestartSeconds, note)
	return &api.CountResp{N: 1, Note: note}, nil, nil
}

// yieldTaken records that an offered lease's resource was stopped by reclaim.
func (m *Manager) yieldTaken(resourceID string, now time.Time) {
	for _, y := range m.st.Yields {
		if y.Taken == nil && y.Verdict == "" {
			for _, rid := range y.Resources {
				if rid == resourceID {
					t := now
					y.Taken = &t
				}
			}
		}
	}
}

// yieldsSeeUse is a session using resources again (a command named them, or it asked for a
// lease of the same kind): for an offer that said it was done with them, that was not true;
// for one that was taken, it marks the start of a restart. Must hold mu.
func (m *Manager) yieldsSeeUse(session string, ids []string, kind string, now time.Time) {
	for _, y := range m.st.Yields {
		if y.Session != session || y.Verdict != "" || now.Sub(y.At) > yieldCheck {
			continue
		}
		hit := kind != "" && y.Kind == kind
		for _, id := range ids {
			for _, rid := range y.Resources {
				hit = hit || id == rid
			}
		}
		if !hit {
			continue
		}
		if y.UsedAgainAt == nil {
			t := now
			y.UsedAgainAt = &t
		}
		if y.Taken != nil && y.RestartBegan == nil {
			t := now
			y.RestartBegan = &t
		}
	}
}

// yieldsSeeReturn is a resource appearing again: the end of a restart. Must hold mu.
func (m *Manager) yieldsSeeReturn(r *model.Resource, now time.Time) {
	for _, y := range m.st.Yields {
		if y.Taken == nil || y.RestartedAt != nil || now.Sub(y.At) > yieldCheck {
			continue
		}
		for _, rid := range y.Resources {
			if rid != r.ID {
				continue
			}
			t := now
			y.RestartedAt = &t
			began := *y.Taken
			if y.RestartBegan != nil {
				began = *y.RestartBegan
			}
			if secs := now.Sub(began).Seconds(); secs > 0 {
				m.learnRestart(r.Kind, secs)
			}
		}
	}
}

// learnRestart folds a measured restart into the per-kind estimate (a moving average).
func (m *Manager) learnRestart(kind string, secs float64) {
	if m.st.RestartSeconds == nil {
		m.st.RestartSeconds = map[string]float64{}
	}
	if old, ok := m.st.RestartSeconds[kind]; ok {
		m.st.RestartSeconds[kind] = 0.7*old + 0.3*secs
	} else {
		m.st.RestartSeconds[kind] = secs
	}
}

// judgeYields decides, once enough time has passed, whether each offer was true, and updates
// its session's record. Must hold mu.
func (m *Manager) judgeYields(now time.Time) {
	for _, y := range m.st.Yields {
		if y.Verdict != "" {
			continue
		}
		var wrong string
		switch {
		case y.Cost == "none" && y.UsedAgainAt != nil:
			wrong = fmt.Sprintf("said it was done with it, then used it again %s later", y.UsedAgainAt.Sub(y.At).Round(time.Second))
		case y.Cost == "low" && y.RestartBegan != nil && y.RestartedAt != nil && y.RestartSeconds > 0 &&
			y.RestartedAt.Sub(*y.RestartBegan).Seconds() > 3*float64(y.RestartSeconds)+30:
			wrong = fmt.Sprintf("said it could be back in %ds; it took %s", y.RestartSeconds, y.RestartedAt.Sub(*y.RestartBegan).Round(time.Second))
		}
		if wrong == "" && now.Sub(y.At) < yieldCheck {
			continue
		}
		s := m.st.Sessions[y.Session]
		if wrong == "" {
			y.Verdict = "true"
			if s != nil {
				s.YieldReports++
			}
			continue
		}
		y.Verdict = "false"
		if s == nil {
			continue
		}
		s.YieldReports++
		s.YieldMisreports++
		m.event("MISREPORT lease %s of %s: %s. Its later answers count for less (honesty %.0f%%).",
			y.Lease, short(y.Session), wrong, 100*s.Honesty())
		if s.Honesty() < 0.4 {
			s.DistrustUntil = now.Add(24 * time.Hour)
			m.event("DISTRUST %s until %s: its answers and memory figures are not taken at their word", short(y.Session), s.DistrustUntil.Format("Jan 2 15:04"))
		}
	}
}

// sortByOffer orders reclaim candidates: offers first, cheapest first, then the usual order.
func sortCands[T any](cands []T, rank func(T) int, rest func(a, b T) bool) {
	sort.SliceStable(cands, func(i, j int) bool {
		if ri, rj := rank(cands[i]), rank(cands[j]); ri != rj {
			return ri < rj
		}
		return rest(cands[i], cands[j])
	})
}

// stoppedTouchedBy is which recently stopped resources a command names: using something that
// was stopped is using it again.
func (m *Manager) stoppedTouchedBy(cmd, cwd string) []string {
	gone := map[string]*model.Resource{}
	for _, st := range m.st.Stopped {
		if m.now().Sub(st.StoppedAt) < yieldCheck {
			r := st.Resource
			gone[r.ID] = &r
		}
	}
	if len(gone) == 0 {
		return nil
	}
	return evidence.TouchedBy(cmd, cwd, gone, "")
}
