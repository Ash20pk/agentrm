package manager

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"agentrm/internal/model"
)

// Receipts. The agent that asks for memory knows its project best, so the figure it asks for
// is the figure it is charged (AgentRM's own estimate, memory.go, only fills in when it asks for none). What AgentRM
// adds is the proof: when a lease ends, what the work really used is written to a plain text
// file per project, one line per lease, that any agent can read before it asks next time, and
// a request that was far off is answered in the agent's conversation with the figure to ask for
// instead. AgentRM writes the file because a receipt the asker writes itself proves nothing.

// How far use may be from the ask before the agent is told to change its figure.
const (
	receiptUnder = 0.6  // used under 60% of what it asked
	receiptOver  = 1.25 // used over 125% of what it asked
)

var receiptName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// receiptsPath is the receipts file for the project at cwd.
func (m *Manager) receiptsPath(cwd string) string {
	p := projectOf(cwd)
	if p == "" {
		p = "other"
	}
	return filepath.Join(m.dir, "receipts", receiptName.ReplaceAllString(p, "_")+".txt")
}

// suggestGB is what to ask for next time after using usedMB: what it used and a fifth more,
// rounded up to a tenth of a GB.
func suggestGB(usedMB float64) float64 {
	return math.Ceil(usedMB*1.2/1024*10) / 10
}

// writeReceipt records what a finished lease asked for and used, and tells its session when the
// ask was far off. Must hold mu.
func (m *Manager) writeReceipt(l *model.Lease, kind string, now time.Time) {
	if kind == "" || l.PeakMB < minLearnMB || len(l.Resources) > 0 || l.HadResources || inferred(l) || l.Session == "you" {
		return
	}
	asked, used := l.MemGB, l.PeakMB/1024
	who := "asked"
	if !l.Declared {
		who = "estimated by AgentRM"
	}
	path := m.receiptsPath(l.Cwd)
	if m.dir != "" {
		line := fmt.Sprintf("%s  %-12s  %s %.1f GB  used %.1f GB  next time ask %.1f GB  %s\n",
			now.Format("2006-01-02 15:04"), kind, who, asked, used, suggestGB(l.PeakMB), trunc(oneLine(l.Cmd), 80))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
			if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
				if fi, _ := f.Stat(); fi != nil && fi.Size() == 0 {
					fmt.Fprintf(f, "# What %s's leases asked for and really used, written by AgentRM when each ended.\n"+
						"# Read this before asking: agentrm acquire --kind <kind> --mem <GB>\n", strings.TrimSuffix(filepath.Base(path), ".txt"))
				}
				_, _ = f.WriteString(line)
				f.Close()
			}
		}
	}
	if used >= receiptUnder*asked && used <= receiptOver*asked {
		return
	}
	verb := "asked for"
	if !l.Declared {
		verb = "was charged AgentRM's estimate of"
	}
	m.st.Questions["receipt:"+l.ID] = &model.Question{Created: now, For: l.Session,
		Delivered: map[string]bool{}, Answers: map[string]string{},
		Text: fmt.Sprintf("[agentrm] Receipt: your %s %s %.1f GB and used %.1f GB. Next time ask for about %.1f GB: "+
			"agentrm acquire --kind %s --mem %.1f. Every lease of this project is in %s.",
			kind, verb, asked, used, suggestGB(l.PeakMB), kind, suggestGB(l.PeakMB), path)}
	m.event("RECEIPT %s %s %s %.1f GB, used %.1f GB (%s)", l.ID, kind, verb, asked, used, short(l.Session))
}
