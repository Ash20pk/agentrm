package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseBool(t *testing.T) {
	for _, s := range []string{"true", "TRUE", "yes", "Y", "1", "on", " On "} {
		if v, err := parseBool(s); err != nil || !v {
			t.Errorf("%q: %v %v", s, v, err)
		}
	}
	for _, s := range []string{"false", "False", "no", "N", "0", "off"} {
		if v, err := parseBool(s); err != nil || v {
			t.Errorf("%q: %v %v", s, v, err)
		}
	}
	for _, s := range []string{"", "maybe", "2", "tru", "enable"} {
		if _, err := parseBool(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}

func TestConfigRejectsUnknownBooleans(t *testing.T) {
	// Refused before any call to the daemon: exit 1 with nothing sent.
	t.Setenv("AGENTRM_SOCK", filepath.Join(t.TempDir(), "none.sock"))
	if code := runCLI("config", []string{"--dry-run=maybe"}); code != 1 {
		t.Errorf("--dry-run=maybe: exit %d", code)
	}
	if code := runCLI("config", []string{"--auto-stop", "idle_lease=sometimes"}); code != 1 {
		t.Errorf("--auto-stop idle_lease=sometimes: exit %d", code)
	}
	if code := runCLI("config", []string{"--auto-stop", "idle_lease"}); code != 1 {
		t.Errorf("--auto-stop without a value: exit %d", code)
	}
}

func TestExecutableReplacementIsNoticed(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "agentrm")
	if err := os.WriteFile(exe, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	orig, _ := os.Stat(exe)
	if why := exeChanged(exe, orig); why != "" {
		t.Fatalf("unchanged file reported: %s", why)
	}
	// Replaced the way an update or a build does it: a new file renamed over the old one.
	tmp := filepath.Join(dir, "agentrm.new")
	_ = os.WriteFile(tmp, []byte("v2"), 0o755)
	if err := os.Rename(tmp, exe); err != nil {
		t.Fatal(err)
	}
	if why := exeChanged(exe, orig); why == "" {
		t.Fatal("replacement not noticed")
	}
	_ = os.Remove(exe)
	if why := exeChanged(exe, orig); why == "" {
		t.Fatal("removal not noticed")
	}

	// The watcher calls back once and stops.
	_ = os.WriteFile(exe, []byte("v3"), 0o755)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	called := make(chan string, 2)
	go watchExecutable(ctx, exe, 10*time.Millisecond, func(why string) { called <- why })
	time.Sleep(30 * time.Millisecond)
	_ = os.WriteFile(tmp, []byte("v4!"), 0o755)
	_ = os.Rename(tmp, exe)
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("the watcher did not notice the new binary")
	}
}
