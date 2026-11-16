// Package store keeps AgentRM's state and config on disk.
//
// Writes are atomic (temp file, fsync, rename, fsync the directory), so a crash leaves either
// the old file or the new one, never a torn one. They are debounced: the manager marks the
// state dirty and a writer flushes at most once a second, and on shutdown (D2).
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"agentrm/internal/model"
)

// Dir is AgentRM's directory, ~/.agentrm unless AGENTRM_HOME says otherwise.
func Dir() string {
	if d := os.Getenv("AGENTRM_HOME"); d != "" {
		return d
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".agentrm")
}

func SockPath() string {
	if s := os.Getenv("AGENTRM_SOCK"); s != "" {
		return s
	}
	return filepath.Join(Dir(), "agentrm.sock")
}

// WriteAtomic writes b to path so that a crash never leaves a partial file.
func WriteAtomic(path string, b []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// ---- config ----

// LoadConfig reads config.json over the defaults. On first run it writes the defaults, sized
// to this machine's memory. A config.json that does not parse is kept aside, not overwritten.
func LoadConfig(dir string, totalMB float64) (model.Config, []string) {
	var notes []string
	cfg := model.DefaultConfig()
	p := filepath.Join(dir, "config.json")
	b, err := os.ReadFile(p)
	switch {
	case os.IsNotExist(err):
		model.ScaleToMachine(&cfg, totalMB)
		if err := SaveConfig(dir, cfg); err != nil {
			notes = append(notes, "could not write config.json: "+err.Error())
		}
	case err != nil:
		notes = append(notes, "could not read config.json, using defaults: "+err.Error())
	default:
		if err := json.Unmarshal(b, &cfg); err != nil {
			bad := p + ".invalid-" + time.Now().Format("20060102-150405")
			_ = os.Rename(p, bad)
			cfg = model.DefaultConfig()
			model.ScaleToMachine(&cfg, totalMB)
			_ = SaveConfig(dir, cfg)
			notes = append(notes, fmt.Sprintf("config.json did not parse (%v); moved it to %s and wrote fresh defaults", err, bad))
		}
	}
	return cfg, notes
}

func SaveConfig(dir string, cfg model.Config) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return WriteAtomic(filepath.Join(dir, "config.json"), append(b, '\n'), 0o644)
}

// ---- state ----

func statePath(dir string) string { return filepath.Join(dir, "state.json") }

// LoadState reads state.json and migrates it to the current version. A state file that does
// not parse is kept aside and a fresh state is used: the daemon rebuilds most of it from what
// is running within one sweep.
func LoadState(dir string, now time.Time) (model.State, []string) {
	b, err := os.ReadFile(statePath(dir))
	if os.IsNotExist(err) {
		return model.NewState(), nil
	}
	if err != nil {
		return model.NewState(), []string{"could not read state.json: " + err.Error()}
	}
	var st model.State
	if err := json.Unmarshal(b, &st); err != nil {
		bad := statePath(dir) + ".invalid-" + time.Now().Format("20060102-150405")
		_ = os.Rename(statePath(dir), bad)
		return model.NewState(), []string{fmt.Sprintf("state.json did not parse (%v); moved it to %s and started fresh", err, bad)}
	}
	notes := Migrate(&st, now)
	return st, notes
}

// Saver writes the state at most once per interval, and on Flush.
type Saver struct {
	dir      string
	mu       sync.Mutex
	pending  []byte
	interval time.Duration
	stop     chan struct{}
	done     chan struct{}
	onErr    func(error)
}

func NewSaver(dir string, interval time.Duration, onErr func(error)) *Saver {
	s := &Saver{dir: dir, interval: interval, stop: make(chan struct{}), done: make(chan struct{}), onErr: onErr}
	go s.loop()
	return s
}

// Queue hands over a serialized state to be written. Only the latest one is kept.
func (s *Saver) Queue(b []byte) {
	s.mu.Lock()
	s.pending = b
	s.mu.Unlock()
}

func (s *Saver) loop() {
	defer close(s.done)
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.Flush()
		case <-s.stop:
			s.Flush()
			return
		}
	}
}

// Flush writes any queued state now.
func (s *Saver) Flush() {
	s.mu.Lock()
	b := s.pending
	s.pending = nil
	s.mu.Unlock()
	if b == nil {
		return
	}
	if err := WriteAtomic(statePath(s.dir), b, 0o600); err != nil && s.onErr != nil {
		s.onErr(err)
	}
}

// Close flushes and stops the writer.
func (s *Saver) Close() {
	close(s.stop)
	<-s.done
}
