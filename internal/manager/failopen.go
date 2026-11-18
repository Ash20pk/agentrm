package manager

import (
	"fmt"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/model"
)

// When AgentRM cannot be sure, it gets out of the way. A resource manager that blocks work on
// numbers it cannot trust does more harm than the memory it saves, so:
//
//   - you can let everything through for a while (agentrm bypass, or the app);
//   - if memory could not be read, or the daemon has not swept for minutes, every request is
//     granted;
//   - a queued request is granted after max_wait_minutes unless the Mac is really swapping:
//     estimates only decide the order, the machine's own pressure decides whether to refuse.
//
// While AgentRM is open, nothing is paused or stopped automatically either.

const staleSweep = 3 * time.Minute

// failOpen says why AgentRM lets everything through right now, or "". Must hold mu.
func (m *Manager) failOpen(now time.Time) string {
	switch {
	case m.st.BypassUntil != nil && now.Before(*m.st.BypassUntil):
		return "you let everything through until " + m.st.BypassUntil.Format("15:04")
	case m.mem.TotalMB > 0 && m.mem.AvailableMB == 0 && m.mem.WiredMB == 0 && m.mem.CompressedMB == 0:
		return "AgentRM could not read memory use, so it is not refusing anything"
	case m.firstDone && !m.lastSweep.IsZero() && now.Sub(m.lastSweep) > staleSweep:
		return fmt.Sprintf("AgentRM has not measured the Mac for %s, so it is not refusing anything", now.Sub(m.lastSweep).Round(time.Minute))
	}
	return ""
}

// maxWait is how long a queued request waits before it is granted anyway (0: never).
func (m *Manager) maxWait() time.Duration {
	switch n := m.cfg.MaxWaitMinutes; {
	case n < 0:
		return 0
	case n == 0:
		return 15 * time.Minute
	default:
		return minutes(n)
	}
}

// waitedEnough says whether a queued request has waited long enough to be granted on the
// Mac's real state alone.
func (m *Manager) waitedEnough(w *model.Waiter, now time.Time) bool {
	if mw := m.maxWait(); mw == 0 || w.Retry || now.Sub(w.Since) < mw {
		return false
	}
	bad, _ := m.thrashing()
	return !bad
}

// bypass answers `agentrm bypass [minutes]`: you let everything through for a while (0 ends
// it). Only you can: an agent that could would simply let itself through.
func (m *Manager) bypass(c api.Caller, r api.Req) (any, []Effect, error) {
	if !c.Human {
		return nil, nil, fmt.Errorf("only you can let everything through; an agent cannot")
	}
	now := m.now()
	n := r.UntilMinutes
	if n <= 0 {
		m.st.BypassUntil = nil
		m.event("BYPASS ended by %s", c.Session)
		return &api.BypassResp{}, nil, nil
	}
	n = min(n, 24*60)
	until := now.Add(minutes(n))
	m.st.BypassUntil = &until
	m.event("BYPASS everything is let through until %s (by %s)", until.Format("15:04"), c.App)
	// Whoever is queued gets in now.
	m.grantWaiters(now, false)
	return &api.BypassResp{Until: until}, nil, nil
}
