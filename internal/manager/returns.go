package manager

import (
	"time"

	"agentrm/internal/model"
)

// What stopping something gives back. A process that exits returns its memory to macOS. A
// container does not, necessarily: it frees memory inside Docker's VM, and the VM keeps much of
// it. Counting a container's full size when reclaiming for a blocked request stops several
// times more than the request needs, and leaves it waiting. So the share that really comes back
// is measured after each stop and learned per source.

// returnDefault is assumed for a VM-backed source until returnLearnAfter stops are measured.
const (
	returnDefault    = 0.5
	returnLearnAfter = 3
	returnSettle     = 60 * time.Second // how long after a stop the gain is read
)

// returnProbe is one stop (or a burst of stops) whose effect on available memory is pending.
type returnProbe struct {
	source  string
	at      time.Time
	mb      float64 // counted memory stopped
	availMB float64 // available memory just before
}

// vmBacked sources run their resources inside a VM.
func vmBacked(source string) bool { return source == "docker" }

// returnRatio is the share of a resource's counted memory that stopping it gives back to macOS.
func (m *Manager) returnRatio(source string) float64 {
	if !vmBacked(source) {
		return 1
	}
	n := float64(m.st.ReturnN[source])
	learned := m.st.ReturnRatio[source]
	if n >= returnLearnAfter {
		return learned
	}
	return (n*learned + (returnLearnAfter-n)*returnDefault) / returnLearnAfter
}

// noteStopped starts measuring what a stop gives back. Stops of one source close together are
// one probe: they free memory together and cannot be told apart.
func (m *Manager) noteStopped(r model.Resource, now time.Time) {
	if !vmBacked(r.Source) || r.MemMB < 200 {
		return
	}
	for _, p := range m.returnProbes {
		if p.source == r.Source && now.Sub(p.at) < returnSettle/2 {
			p.mb += r.MemMB
			return
		}
	}
	m.returnProbes = append(m.returnProbes, &returnProbe{source: r.Source, at: now, mb: r.MemMB, availMB: m.mem.AvailableMB})
}

// measureReturns reads what pending stops gave back, once they have settled.
func (m *Manager) measureReturns(now time.Time) {
	keep := m.returnProbes[:0]
	for _, p := range m.returnProbes {
		if now.Sub(p.at) < returnSettle {
			keep = append(keep, p)
			continue
		}
		got := min(max((m.mem.AvailableMB-p.availMB)/p.mb, 0.05), 1)
		if m.st.ReturnRatio == nil {
			m.st.ReturnRatio, m.st.ReturnN = map[string]float64{}, map[string]int{}
		}
		if m.st.ReturnN[p.source] == 0 {
			m.st.ReturnRatio[p.source] = got
		} else {
			m.st.ReturnRatio[p.source] = 0.7*m.st.ReturnRatio[p.source] + 0.3*got
		}
		m.st.ReturnN[p.source]++
		m.event("RETURNED stopping %.1f GB on %s gave back %.1f GB to macOS (%.0f%%); now counting %.0f%%",
			p.mb/1024, p.source, max(0, m.mem.AvailableMB-p.availMB)/1024, got*100, m.returnRatio(p.source)*100)
	}
	m.returnProbes = keep
}
