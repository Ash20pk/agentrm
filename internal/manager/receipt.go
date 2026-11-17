package manager

// The receipt: what AgentRM did over a period, computed from what it already keeps (the
// decision log, what it stopped, and the events log on disk), with no bookkeeping of its own.
// Once a week it is sent as one notification; the app shows it in full.

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"agentrm/internal/api"
)

var queueWait = regexp.MustCompile(`\(from the queue, after ([0-9hms.]+)\)`)

// eventLines reads the events log (and its rotated files) from since on, oldest first.
func (m *Manager) eventLines(since time.Time) []struct {
	t    time.Time
	kind string
	text string
} {
	var out []struct {
		t    time.Time
		kind string
		text string
	}
	if m.dir == "" {
		for _, e := range m.events { // tests, and replay: what is in memory
			if !e.Time.Before(since) {
				out = append(out, struct {
					t    time.Time
					kind string
					text string
				}{e.Time, e.Kind, e.Text})
			}
		}
		return out
	}
	base := filepath.Join(m.dir, "events.log")
	files := []string{}
	for i := 9; i >= 1; i-- {
		if _, err := os.Stat(base + "." + strconv.Itoa(i)); err == nil {
			files = append(files, base+"."+strconv.Itoa(i))
		}
	}
	files = append(files, base)
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			l := sc.Text()
			if len(l) < 20 {
				continue
			}
			t, err := time.ParseInLocation("2006-01-02 15:04:05", l[:19], time.Local)
			if err != nil || t.Before(since) {
				continue
			}
			text := l[20:]
			kind, _, _ := strings.Cut(text, " ")
			out = append(out, struct {
				t    time.Time
				kind string
				text string
			}{t, kind, text})
		}
		fh.Close()
	}
	return out
}

func (m *Manager) makeReceipt(since, until time.Time) *api.Receipt {
	r := &api.Receipt{Since: since, Until: until}
	for _, d := range m.st.Decisions {
		if d.Time.Before(since) || d.Time.After(until) || d.Action == "reclaim" {
			continue
		}
		switch {
		case d.DryRun && d.Action == "stop":
			r.WouldStop++
			r.WouldFreeGB += d.MemMB / 1024
		case d.DryRun && d.Action == "pause":
			// a would-pause leads to a would-stop only after a real pause; count it as would-stop
			r.WouldStop++
			r.WouldFreeGB += d.MemMB / 1024
		case d.Action == "pause":
			r.Paused++
		}
		if d.Verdict == "wrong" {
			r.Wrong++
		}
	}
	for _, s := range m.st.Stopped {
		if !s.StoppedAt.Before(since) && !s.StoppedAt.After(until) {
			r.Stopped++
			r.FreedGB += s.Resource.MemMB / 1024
		}
	}
	var waits []time.Duration
	for _, e := range m.eventLines(since) {
		if e.t.After(until) {
			continue
		}
		switch e.kind {
		case "ENDED":
			r.SessionsEnded++
		case "LEFTOVER":
			r.Leftovers++
		case "RESTORED":
			r.Restored++
		case "MISREPORT":
			r.Misreports++
		case "GRANT":
			if mm := queueWait.FindStringSubmatch(e.text); mm != nil {
				if d, err := time.ParseDuration(mm[1]); err == nil {
					waits = append(waits, d)
				}
			}
		}
	}
	r.Queued = len(waits)
	if len(waits) > 0 {
		var sum time.Duration
		for _, w := range waits {
			sum += w
			r.WaitMax = max(r.WaitMax, w)
		}
		r.WaitMean = sum / time.Duration(len(waits))
	}
	r.Text = receiptText(r)
	return r
}

// plural is "1 thing" or "3 things".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// friendly writes a duration as people say it: "1 min 14 s", "3 h 5 min".
func friendly(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%d h %d min", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%d min %d s", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%d s", int(d.Seconds()))
}

func receiptText(r *api.Receipt) string {
	var parts []string
	if r.Stopped > 0 {
		parts = append(parts, fmt.Sprintf("stopped %s, freeing %.1f GB", plural(r.Stopped, "thing", "things"), r.FreedGB))
	}
	if r.WouldStop > 0 {
		parts = append(parts, fmt.Sprintf("would have stopped %s (%.1f GB) in dry run", plural(r.WouldStop, "thing", "things"), r.WouldFreeGB))
	}
	if r.Leftovers > 0 {
		parts = append(parts, fmt.Sprintf("found %s that ended sessions left running", plural(r.Leftovers, "process", "processes")))
	}
	if r.Queued > 0 {
		parts = append(parts, fmt.Sprintf("%s for memory, %s on average", plural(r.Queued, "request waited", "requests waited"), friendly(r.WaitMean)))
	}
	if r.Restored > 0 {
		parts = append(parts, fmt.Sprintf("%s afterwards", plural(r.Restored, "was restored", "were restored")))
	}
	if r.Wrong > 0 {
		parts = append(parts, fmt.Sprintf("%s you marked wrong", plural(r.Wrong, "decision", "decisions")))
	}
	if len(parts) == 0 {
		return "A quiet week: nothing needed collecting."
	}
	s := strings.Join(parts, "; ")
	return strings.ToUpper(s[:1]) + s[1:] + "."
}

// receipt answers `agentrm receipt [--days N]`.
func (m *Manager) receipt(c api.Caller, r api.Req) (any, []Effect, error) {
	days := r.Days
	if days <= 0 {
		days = 7
	}
	now := m.now()
	return m.makeReceipt(now.AddDate(0, 0, -days), now), nil, nil
}

// weeklyReceipt sends the receipt once a week. Must hold mu.
func (m *Manager) weeklyReceipt(now time.Time) {
	if m.st.LastReceipt.IsZero() {
		m.st.LastReceipt = now // the first week starts now
		return
	}
	if now.Sub(m.st.LastReceipt) < 7*24*time.Hour {
		return
	}
	r := m.makeReceipt(m.st.LastReceipt, now)
	m.st.LastReceipt = now
	m.event("RECEIPT for the week: %s", r.Text)
	m.notify("AgentRM this week", r.Text)
}
