package trace

import (
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentrm/internal/model"
	"agentrm/internal/sys"
)

func TestRoundTripWithDeltas(t *testing.T) {
	dir := t.TempDir()
	w := NewWriter(dir, 36500) // the frames are dated in the past; the writer prunes by the real clock
	t0 := time.Date(2026, 11, 17, 10, 0, 0, 0, time.Local)
	owners := map[int]Owner{700: {Agent: "claude", Session: "s", AgentPID: 500}}
	owner := func(p *sys.Proc) (Owner, bool) { o, ok := owners[p.PID]; return o, ok }
	fp := func(pid int) (float64, bool) {
		if pid == 700 {
			return 900, true
		}
		return 0, false
	}
	tab := map[int]*sys.Proc{
		1:   {PID: 1, Exe: "/sbin/launchd"},
		500: {PID: 500, PPID: 1, Exe: "/usr/local/bin/claude", Args: "claude", Start: t0, RSSMB: 300.4},
		700: {PID: 700, PPID: 1, Exe: "node", Args: "node server.js --token=abc123 --port 3000", Start: t0, RSSMB: 400},
	}
	for i := 0; i < 3; i++ {
		now := t0.Add(time.Duration(i) * 30 * time.Second)
		if i == 2 {
			delete(tab, 500) // the agent exits
			tab[700].RSSMB = 50
		}
		w.Write(Frame{T: now, Sweep: &Sweep{Procs: w.Procs(now, tab, owner, fp), Found: []*model.Resource{{ID: "srv:3000"}}}})
	}
	w.Close()

	rd, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var frames []*Frame
	var last map[int]*sys.Proc
	for {
		f, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, f)
		last = rd.Table()
	}
	if len(frames) != 3 {
		t.Fatalf("%d frames", len(frames))
	}
	if !frames[0].Sweep.Procs.Full || len(frames[1].Sweep.Procs.Set) != 0 {
		t.Fatalf("want a whole table, then nothing for an unchanged one: %+v", frames[1].Sweep.Procs)
	}
	if last[500] != nil || last[700] == nil || last[700].RSSMB != 50 {
		t.Fatalf("table not rebuilt from deltas: %+v", last)
	}
	if o, ok := rd.Owner(700); !ok || o.Session != "s" {
		t.Fatalf("owner lost: %+v", o)
	}
	if v, _ := rd.Footprint(700); v != 900 {
		t.Fatalf("footprint lost: %v", v)
	}
	if strings.Contains(last[700].Args, "abc123") || !strings.Contains(last[700].Args, "--port 3000") {
		t.Fatalf("secret kept or command mangled: %q", last[700].Args)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*.jsonl.gz")); len(m) != 1 {
		t.Fatalf("files: %v", m)
	}
}

func TestRedact(t *testing.T) {
	for in, want := range map[string]string{
		"curl -H 'Authorization: Bearer abc' x":       "[redacted]",
		"psql --password=hunter2":                     "[redacted]",
		"node app.js API_KEY=sk_live_123":             "[redacted]",
		"cat /Users/ash/a/very/long/path/to/file":     "/Users/ash/a/very/long/path/to/file",
		"run eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9xyz": "[redacted]",
	} {
		if got := Redact(in); !strings.Contains(got, want) {
			t.Errorf("Redact(%q) = %q, want it to contain %q", in, got, want)
		}
	}
	if got := Redact("psql --password=hunter2"); strings.Contains(got, "hunter2") {
		t.Errorf("password kept: %q", got)
	}
}

func TestCrashedFileDoesNotSpoilTheNext(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Date(2026, 11, 17, 10, 0, 0, 0, time.Local)
	w := NewWriter(dir, 36500) // the frames are dated in the past; the writer prunes by the real clock
	w.Write(Frame{T: t0, Exit: 1})
	w.Flush() // then the daemon dies: no gzip trailer
	w2 := NewWriter(dir, 36500) // the frames are dated in the past; the writer prunes by the real clock
	w2.Write(Frame{T: t0.Add(time.Minute), Exit: 2})
	w2.Close()
	rd, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var exits []int
	for {
		f, err := rd.Next()
		if err != nil {
			break
		}
		exits = append(exits, f.Exit)
	}
	if len(exits) != 2 || exits[0] != 1 || exits[1] != 2 {
		t.Fatalf("frames after a crash were lost: %v", exits)
	}
}
