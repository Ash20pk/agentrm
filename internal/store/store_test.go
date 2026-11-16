package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentrm/internal/model"
)

func TestMigrateFromVersion1(t *testing.T) {
	dir := t.TempDir()
	old := `{"leases":{"a":{"id":"a","session":"s","kind":"cluster"}},"learned_mb":{"cluster":0.4,"cargo-build":2048},
	         "resources":{"kind:x":{"id":"kind:x","orphan_since":"2026-01-01T00:00:00Z"}}}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	st, notes := LoadState(dir, now)
	if st.Version != model.StateVersion || len(notes) != 2 {
		t.Fatalf("version %d, notes %v", st.Version, notes)
	}
	if !st.Leases["a"].LastUsed.Equal(now) || st.Leases["a"].Evidence == "" {
		t.Error("v1 lease not migrated")
	}
	if _, ok := st.Learned["cluster"]; ok || st.Learned["cargo-build"] != 2048 {
		t.Errorf("learned not cleaned: %v", st.Learned)
	}
	if st.Resources["kind:x"].OrphanSince != nil {
		t.Error("orphan clock survived a restart")
	}
	if st.Approvals == nil || st.Touches == nil {
		t.Error("maps not initialised")
	}
}

func TestBadStateIsKeptAside(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "state.json"), []byte("{not json"), 0o600)
	st, notes := LoadState(dir, time.Now())
	if st.Leases == nil || len(notes) != 1 || !strings.Contains(notes[0], "did not parse") {
		t.Fatalf("notes %v", notes)
	}
	m, _ := filepath.Glob(filepath.Join(dir, "state.json.invalid-*"))
	if len(m) != 1 {
		t.Fatal("the unreadable state was not kept aside")
	}
}

func TestSaverDebouncesAndFlushesOnClose(t *testing.T) {
	dir := t.TempDir()
	s := NewSaver(dir, time.Hour, nil) // never ticks during the test
	s.Queue([]byte(`{"version":1}`))
	s.Queue([]byte(`{"version":2}`))
	if _, err := os.Stat(filepath.Join(dir, "state.json")); err == nil {
		t.Fatal("written before the interval")
	}
	s.Close()
	b, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if string(b) != `{"version":2}` {
		t.Fatalf("got %q, want the latest queued state", b)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, ".state.json.tmp-*")); len(m) != 0 {
		t.Fatal("temp file left behind")
	}
}

func TestConfigFirstRunIsScaledAndBadConfigKept(t *testing.T) {
	dir := t.TempDir()
	cfg, _ := LoadConfig(dir, 64*1024)
	if cfg.HeadroomGB != 3 {
		t.Errorf("64 GB headroom = %v, want 3", cfg.HeadroomGB)
	}
	// Later loads keep what the file says, even on a different machine size.
	cfg2, _ := LoadConfig(dir, 8*1024)
	if cfg2.HeadroomGB != 3 {
		t.Errorf("config re-scaled on load: %v", cfg2.HeadroomGB)
	}
	_ = os.WriteFile(filepath.Join(dir, "config.json"), []byte("{oops"), 0o644)
	_, notes := LoadConfig(dir, 16*1024)
	if len(notes) != 1 {
		t.Fatalf("bad config not reported: %v", notes)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "config.json.invalid-*")); len(m) != 1 {
		t.Fatal("bad config not kept aside")
	}
}

func TestRotatingFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "events.log")
	r := NewRotatingFile(p, 100, 2)
	for i := 0; i < 30; i++ {
		_, _ = r.Write([]byte("0123456789\n"))
	}
	r.Close()
	for _, f := range []string{p, p + ".1", p + ".2"} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s missing", f)
		}
	}
	if _, err := os.Stat(p + ".3"); err == nil {
		t.Error("kept more files than asked")
	}
	if fi, _ := os.Stat(p); fi.Size() > 100 {
		t.Errorf("current file is %d bytes, over the limit", fi.Size())
	}
}
