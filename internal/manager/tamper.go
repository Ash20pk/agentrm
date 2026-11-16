package manager

// Tamper detection. Agents run as you, so they can edit AgentRM's files, kill its daemon or
// remove its hooks; AgentRM cannot prevent that without running as root (see D7). It can make
// it visible: each of these raises an event, which the app turns into a notification.

import (
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"agentrm/internal/model"
	"agentrm/internal/setup"
	"agentrm/internal/store"
)

type tamperState struct {
	cfgHash [32]byte
	// ours holds the last state hashes the daemon produced. The saver writes up to a second
	// after the state is queued, so the file may legitimately hold any of them.
	ours      [8][32]byte
	oursNext  int
	hooksSeen bool
}

func (t *tamperState) addOurs(h [32]byte) {
	t.ours[t.oursNext%len(t.ours)] = h
	t.oursNext++
}

func (t *tamperState) isOurs(h [32]byte) bool {
	if t.oursNext == 0 {
		return true // nothing written yet this run: whatever is there was loaded at start
	}
	for _, o := range t.ours {
		if o == h {
			return true
		}
	}
	return false
}

func (m *Manager) markerPath() string { return filepath.Join(m.dir, "daemon.running") }

// tamperInit notices an unclean previous shutdown and records the files as the daemon left them.
func (m *Manager) tamperInit() {
	if fi, err := os.Stat(m.markerPath()); err == nil {
		m.event("TAMPER the daemon stopped unexpectedly last time (around %s): killed or crashed. If you did not do that, an agent may have.",
			fi.ModTime().Format("Jan 2 15:04"))
	}
	_ = os.WriteFile(m.markerPath(), []byte(time.Now().Format(time.RFC3339)), 0o644)
	if b, err := os.ReadFile(filepath.Join(m.dir, "config.json")); err == nil {
		m.tamper.cfgHash = sha256.Sum256(b)
	}
	if b, err := os.ReadFile(filepath.Join(m.dir, "state.json")); err == nil {
		m.tamper.addOurs(sha256.Sum256(b))
	}
	installed, _ := setup.ClaudeHooksInstalled()
	m.tamper.hooksSeen = installed
}

func (m *Manager) tamperCleanShutdown() {
	if m.dir != "" {
		_ = os.Remove(m.markerPath())
	}
}

// tamperWrote records the state as the daemon is about to write it.
func (m *Manager) tamperWrote(b []byte) {
	m.mu.Lock()
	m.tamper.addOurs(sha256.Sum256(b))
	m.mu.Unlock()
}

// tamperCheck looks for changes made behind the daemon's back, and writes settings changed
// through the daemon. Called from the sweep, outside the lock.
func (m *Manager) tamperCheck() {
	if m.saver == nil {
		return
	}
	// Settings changed through the daemon are written here, not under the lock.
	m.mu.Lock()
	cfg, cfgDirty := m.cfg, m.cfgDirty
	m.cfgDirty = false
	m.mu.Unlock()
	if cfgDirty {
		if err := store.SaveConfig(m.dir, cfg); err == nil {
			if b, err := os.ReadFile(filepath.Join(m.dir, "config.json")); err == nil {
				m.mu.Lock()
				m.tamper.cfgHash = sha256.Sum256(b)
				m.mu.Unlock()
			}
		}
	}

	if b, err := os.ReadFile(filepath.Join(m.dir, "config.json")); err == nil {
		h := sha256.Sum256(b)
		m.mu.Lock()
		if h != m.tamper.cfgHash {
			m.tamper.cfgHash = h
			var next model.Config = model.DefaultConfig()
			if err := json.Unmarshal(b, &next); err == nil {
				m.cfg = next
				m.applyLogLevel()
				m.event("OVERRIDE config.json was edited outside AgentRM; the new settings are in effect (dry_run=%v). If you did not do that, an agent may have.", next.DryRun)
			} else {
				m.event("TAMPER config.json was edited outside AgentRM and does not parse; the previous settings stay in effect.")
			}
		}
		m.mu.Unlock()
	}
	if b, err := os.ReadFile(filepath.Join(m.dir, "state.json")); err == nil {
		h := sha256.Sum256(b)
		m.mu.Lock()
		if !m.tamper.isOurs(h) {
			m.tamper.addOurs(h)
			m.dirty = true // the daemon's own copy wins and is written back
			m.event("TAMPER state.json was edited outside AgentRM; the daemon's copy has been written back.")
		}
		m.mu.Unlock()
	}
	installed, _ := setup.ClaudeHooksInstalled()
	m.mu.Lock()
	if m.tamper.hooksSeen && !installed && !setup.RemovedOnPurpose(time.Hour) {
		m.event("TAMPER the Claude Code hooks were removed from ~/.claude/settings.json. Agents are no longer checked. Reinstall them in AgentRM → Settings → Setup.")
	}
	m.tamper.hooksSeen = installed
	m.mu.Unlock()
}
