package manager

// Prediction from the command stream. The pre-tool hook sees every command before it runs, so
// AgentRM can learn, per project, which kind of heavy work follows which (a first-order Markov
// chain over the kinds commands classify to), and how long each kind of lease lives. That gives:
//
//   - an expected end for every lease (status shows it), and a "probably finished" mark once
//     a lease has outlived nine in ten of its kind and is idle;
//   - an early start on making room: when the likely next request of a session would not fit,
//     holders are asked what yielding would cost before the request arrives (asked only;
//     nothing is taken on a guess);
//   - lifetimes for backfilling the queue (admission.go).
//
// The predictor keeps score against the obvious baseline, the project's most frequent kind, so
// it is visible whether it earns its place.

import (
	"fmt"
	"sort"
	"time"

	"agentrm/internal/model"
)

const (
	maxLifetimes = 40 // durations kept per kind and project
	minSamples   = 5  // below this, nothing is predicted
)

func (m *Manager) predictState() *model.Prediction {
	if m.st.Predict == nil {
		m.st.Predict = &model.Prediction{Next: map[string]map[string]map[string]int{}, Lifetimes: map[string][]float64{},
			Last: map[string]string{}}
	}
	return m.st.Predict
}

// predictNext is the most likely next heavy kind for a project after prev, its probability and
// how many observations it rests on.
func (m *Manager) predictNext(project, prev string) (string, float64, int) {
	row := m.predictState().Next[project][prev]
	total, best, bestN := 0, "", 0
	for k, n := range row {
		total += n
		if n > bestN || n == bestN && k < best {
			best, bestN = k, n
		}
	}
	if total == 0 {
		return "", 0, 0
	}
	return best, float64(bestN) / float64(total), total
}

// baselineNext is the project's most frequent kind, whatever came before.
func (m *Manager) baselineNext(project string) string {
	counts := map[string]int{}
	for _, row := range m.predictState().Next[project] {
		for k, n := range row {
			counts[k] += n
		}
	}
	best, bestN := "", 0
	for k, n := range counts {
		if n > bestN || n == bestN && k < best {
			best, bestN = k, n
		}
	}
	return best
}

// observeKind records that a session is about to run heavy work of kind, scoring the
// prediction made for it first. Must hold mu.
func (m *Manager) observeKind(session, cwd, kind string, now time.Time) {
	p := m.predictState()
	project := projectOf(cwd)
	prev := p.Last[session]
	if guess, _, n := m.predictNext(project, prev); n >= minSamples {
		p.Total++
		if guess == kind {
			p.Hits++
		}
		if m.baselineNext(project) == kind {
			p.BaseHits++
		}
	}
	if p.Next[project] == nil {
		p.Next[project] = map[string]map[string]int{}
	}
	if p.Next[project][prev] == nil {
		p.Next[project][prev] = map[string]int{}
	}
	p.Next[project][prev][kind]++
	p.Last[session] = kind
	m.prepareFor(session, project, kind, now)
}

// prepareFor starts making room for a session's likely next request, if it would not fit.
func (m *Manager) prepareFor(session, project, after string, now time.Time) {
	next, prob, n := m.predictNext(project, after)
	if n < minSamples || prob < 0.6 || next == "" || m.findLease(session, next) != nil {
		return
	}
	need := m.estimateGB(next, project) * 1024
	if need <= m.headroomMB() {
		return
	}
	deadline := now.Add(minutes(m.cfg.PressureGraceMinutes))
	asked := 0
	for _, l := range m.st.Leases {
		if l.Session != session && len(l.Resources) > 0 && m.priority(l.Session) <= m.priority(session) {
			m.askYield(l, need, now, deadline)
			asked++
		}
	}
	if asked > 0 {
		m.event("PREDICT %s will likely need %s next (%.0f%% of %d times in %s); %.1f GB does not fit, so %d holder(s) were asked early",
			short(session), next, 100*prob, n, project, need/1024, asked)
	}
}

// learnLifetime records how long a finished lease lived, for its kind in its project.
func (m *Manager) learnLifetime(l *model.Lease, now time.Time) {
	if !l.HadResources && !model.BuildKinds[l.Kind] && l.Kind != model.KHeavy {
		return // granted and never used: says nothing about how long work lasts
	}
	mins := now.Sub(l.Created).Minutes()
	if mins <= 0 {
		return
	}
	p := m.predictState()
	for _, key := range []string{l.Kind + "@" + projectOf(l.Cwd), l.Kind} {
		d := append(p.Lifetimes[key], mins)
		if len(d) > maxLifetimes {
			d = d[len(d)-maxLifetimes:]
		}
		p.Lifetimes[key] = d
	}
}

// lifetime is the learned lifetime of a kind in a project at quantile q (0.5 median, 0.9),
// and whether there is enough to go on.
func (m *Manager) lifetime(kind, cwd string, q float64) (time.Duration, bool) {
	p := m.predictState()
	for _, key := range []string{kind + "@" + projectOf(cwd), kind} {
		d := p.Lifetimes[key]
		if len(d) < minSamples {
			continue
		}
		s := append([]float64(nil), d...)
		sort.Float64s(s)
		i := int(q * float64(len(s)-1))
		return time.Duration(s[i] * float64(time.Minute)), true
	}
	return 0, false
}

// markExpected gives each lease its expected end, and marks one that has outlived nine in ten
// of its kind and is idle as probably finished. Must hold mu.
func (m *Manager) markExpected(now time.Time) {
	for _, l := range m.st.Leases {
		if med, ok := m.lifetime(l.Kind, l.Cwd, 0.5); ok {
			t := l.Created.Add(med)
			l.ExpectedEnd = &t
		}
		p90, ok := m.lifetime(l.Kind, l.Cwd, 0.9)
		likely := ok && now.Sub(l.Created) > p90 && now.Sub(l.LastUsed) > 10*time.Minute
		if likely && !l.LikelyDone {
			m.event("LIKELYDONE lease %s (%s) has run %s, longer than nine in ten of its kind, and is idle",
				l.ID, l.Kind, now.Sub(l.Created).Round(time.Minute))
		}
		l.LikelyDone = likely
	}
}

// PredictionScore is how the predictor is doing, for status.
func (m *Manager) predictionScore() string {
	p := m.predictState()
	if p.Total == 0 {
		return "no predictions scored yet"
	}
	return fmt.Sprintf("next heavy command predicted %d/%d (%.0f%%), baseline %d/%d (%.0f%%)",
		p.Hits, p.Total, 100*float64(p.Hits)/float64(p.Total), p.BaseHits, p.Total, 100*float64(p.BaseHits)/float64(p.Total))
}
