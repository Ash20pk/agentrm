package manager

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"agentrm/internal/model"
)

// Every figure AgentRM acts on is a prediction, and each kind is compared with what really
// happened, continuously, on this Mac:
//
//   - charge:<kind>: what a lease was charged (its MemGB) against the most it used (PeakMB);
//   - return:<source>: what reclaim counted on a stop giving back against what available memory
//     actually rose by;
//   - admission: whether the Mac started swapping hard within grantWatch of a grant (Under
//     counts those: memory that was not there after all);
//   - predict:<kind>: whether a request predicted for a session arrived within prepareWindow
//     (Predicted counts predictions, Actual those that came). It decides whether making room
//     ahead of that kind is allowed at all (prepare.go).
//
// `agentrm status` shows the result, so a figure that drifts from reality is visible within a
// day instead of being discovered when it blocks work.

const grantWatch = 5 * time.Minute

type grantProbe struct {
	at    time.Time
	lease string
	bad   bool
}

// score folds one prediction and its outcome into the ledger. Must hold mu.
func (m *Manager) score(key string, predicted, actual float64, now time.Time) {
	if m.st.Accuracy == nil {
		m.st.Accuracy = map[string]*model.Accuracy{}
	}
	a := m.st.Accuracy[key]
	if a == nil {
		a = &model.Accuracy{Since: now}
		m.st.Accuracy[key] = a
	}
	a.N++
	a.Predicted += predicted
	a.Actual += actual
	d := predicted - actual
	if d < 0 {
		d = -d
	}
	a.AbsErr += d
	switch {
	case predicted > 1.5*actual+128:
		a.Over++
	case actual > 1.5*predicted+128:
		a.Under++
	}
}

// scoreCharge compares a finished lease's charge with what it used. Leases that never ran
// anything measurable (claims of something already running, holds on a cluster that lives in
// Docker's VM) teach nothing about charges.
func (m *Manager) scoreCharge(l *model.Lease, kind string) {
	if kind == "" || l.PeakMB < minLearnMB || len(l.Resources) > 0 || l.HadResources || inferred(l) {
		return
	}
	m.score("charge:"+kind, l.MemGB*1024, l.PeakMB, m.now())
}

// watchGrants marks grants followed by heavy swapping, and scores them once their watch ends.
// Must hold mu.
func (m *Manager) watchGrants(now time.Time) {
	bad, _ := m.thrashing()
	keep := m.grantProbes[:0]
	for _, g := range m.grantProbes {
		if now.Sub(g.at) < grantWatch {
			g.bad = g.bad || bad
			keep = append(keep, g)
			continue
		}
		v := 0.0
		if g.bad {
			v = 1
		}
		if m.st.Accuracy == nil {
			m.st.Accuracy = map[string]*model.Accuracy{}
		}
		a := m.st.Accuracy["admission"]
		if a == nil {
			a = &model.Accuracy{Since: now}
			m.st.Accuracy["admission"] = a
		}
		a.N++
		a.Actual += v
		if g.bad {
			a.Under++
		}
	}
	m.grantProbes = keep
}

// AccuracyLine is the ledger in one line, for status and the receipt.
func AccuracyLine(acc map[string]*model.Accuracy) string {
	var parts []string
	for key, a := range acc {
		if a == nil || a.N == 0 {
			continue
		}
		switch {
		case strings.HasPrefix(key, "charge:") && a.Actual > 0:
			parts = append(parts, fmt.Sprintf("%s charged %.1fx what it used (n=%d, %d over)",
				strings.TrimPrefix(key, "charge:"), a.Predicted/a.Actual, a.N, a.Over))
		case strings.HasPrefix(key, "return:") && a.Predicted > 0:
			parts = append(parts, fmt.Sprintf("%s stops gave back %.0f%% of what was counted (n=%d)",
				strings.TrimPrefix(key, "return:"), 100*a.Actual/a.Predicted, a.N))
		case strings.HasPrefix(key, "predict:"):
			parts = append(parts, fmt.Sprintf("%s predicted ahead came %d/%d (%.0f%%)",
				strings.TrimPrefix(key, "predict:"), int(a.Actual), a.N, 100*a.Actual/float64(a.N)))
		case key == "admission":
			parts = append(parts, fmt.Sprintf("grants followed by heavy swapping %d/%d", a.Under, a.N))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, " · ")
}
