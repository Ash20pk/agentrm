package manager

// Learned stop timing. How long should something unclaimed be left before it is paused? Waiting
// costs the memory it holds for as long as it is held; stopping something that is then needed
// costs the time to bring it back. That is the ski-rental problem, and with no knowledge of the
// future the best guaranteed rule is to stop once what has been paid to hold it equals the cost
// of restarting it (never worse than twice the best possible choice).
//
// AgentRM does better than that when it has seen enough: per resource kind it records how long
// things sat idle before being used again (and which were never used again), and picks the
// wait that minimises the expected cost over that record:
//
//	cost(T) = Σ over idle gaps g:  g ≤ T  →  hold · g            (kept, and used again)
//	                               g > T  →  hold · T + restart   (stopped, then needed)
//	          + never used again:          hold · T
//
// hold is the memory it holds, weighted up under memory pressure; restart is the measured
// restart time of the kind (from yields and restores), at restart_weight_gb gigabytes per
// minute. The configured times stay the bounds: never less than a minute, never more than three
// times grace_minutes, and nothing learned is used before min_timing_samples observations.

import (
	"math"
	"time"

	"agentrm/internal/model"
)

const maxGaps = 300

// defaultRestartSeconds is a starting point per kind, until restarts have been measured.
var defaultRestartSeconds = map[string]float64{
	model.RKCluster: 90, model.RKContainers: 20, model.RKServer: 5, model.RKSimulator: 30, model.RKEmulator: 60,
	model.RKVM: 60, model.RKModel: 20, model.RKDatabase: 5, model.RKBrowser: 3, model.RKGradle: 20, model.RKLeftover: 5,
}

func (m *Manager) restartSeconds(kind string) float64 {
	if s, ok := m.st.RestartSeconds[kind]; ok {
		return s
	}
	if s, ok := defaultRestartSeconds[kind]; ok {
		return s
	}
	return 15
}

func (m *Manager) timingState() map[string][]float64 {
	if m.st.IdleGaps == nil {
		m.st.IdleGaps = map[string][]float64{}
	}
	return m.st.IdleGaps
}

// never marks an idle period that ended with the thing going away unused (JSON has no infinity).
const never = -1.0

// noteIdleGap records that a resource was used again after sitting idle (minutes); never
// records that it went away without being used again.
func (m *Manager) noteIdleGap(kind string, minutes float64) {
	if minutes != never && minutes < 1 {
		return // not an idle period worth learning from
	}
	g := append(m.timingState()[kind], minutes)
	if len(g) > maxGaps {
		g = g[len(g)-maxGaps:]
	}
	m.st.IdleGaps[kind] = g
}

// learnedGrace is the wait before pausing an unclaimed resource of r's kind that minimises
// expected cost, if enough has been seen. pressure weighs memory up when it is short.
func (m *Manager) learnedGrace(r *model.Resource, pressure float64) (time.Duration, bool) {
	if !m.cfg.LearnedTiming {
		return 0, false
	}
	gaps := m.timingState()[r.Kind]
	if len(gaps) < max(1, m.cfg.MinTimingSamples) {
		return 0, false
	}
	hold := max(r.MemMB, 50) / 1024 * pressure                       // GB held, per minute
	restart := m.restartSeconds(r.Kind) / 60 * m.cfg.RestartWeightGB // in GB-minutes
	lo, hi := 1.0, float64(3*max(1, m.cfg.GraceMinutes))
	best, bestCost := hi, math.Inf(1)
	for T := lo; T <= hi; T++ {
		cost := 0.0
		for _, g := range gaps {
			switch {
			case g < 0:
				cost += hold * T
			case g <= T:
				cost += hold * g
			default:
				cost += hold*T + restart
			}
		}
		if cost < bestCost-1e-9 {
			best, bestCost = T, cost
		}
	}
	return time.Duration(best * float64(time.Minute)), true
}

// noteGone records a resource that went away (exited, or was stopped) after sitting idle: as
// far as anyone could tell, it was not going to be used again.
func (m *Manager) noteGone(r *model.Resource, now time.Time) {
	last := r.LastActive
	if last.IsZero() {
		last = r.FirstSeen
	}
	if !last.IsZero() && now.Sub(last) >= time.Minute {
		m.noteIdleGap(r.Kind, never)
	}
}
