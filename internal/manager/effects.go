package manager

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"agentrm/internal/discover"
	"agentrm/internal/model"
)

// Effect is an action on a resource decided under the lock and carried out outside it (D1).
type Effect struct {
	Op  string // pause, resume, stop, restore, restart
	Res model.Resource
	Why string
	By  string // who asked: "agentrm" or a session
	// Stopped is the record a restore was taken from: put back if the restore fails, so that
	// what was stopped can still be found and restored.
	Stopped *model.Stopped
}

type EffectResult struct {
	Effect
	Err error
}

// Executor carries out effects. Tests replace it.
var Executor = func(e Effect) error {
	r := e.Res
	switch e.Op {
	case "pause":
		return discover.Pause(&r)
	case "resume":
		return discover.Resume(&r)
	case "stop":
		return discover.Stop(&r)
	case "restore":
		return discover.Restore(&r)
	case "restart":
		return discover.Restart(&r)
	}
	return nil
}

// RunEffects runs effects in parallel, one per resource.
func RunEffects(effects []Effect) []EffectResult {
	out := make([]EffectResult, len(effects))
	var wg sync.WaitGroup
	for i, e := range effects {
		wg.Add(1)
		go func(i int, e Effect) {
			defer wg.Done()
			out[i] = EffectResult{Effect: e, Err: Executor(e)}
		}(i, e)
	}
	wg.Wait()
	return out
}

func (m *Manager) thawEffect(r *model.Resource, why string) Effect {
	m.pending[r.ID] = "resume"
	return Effect{Op: "resume", Res: *r, Why: why}
}

// applyEffects records what effects did. Must hold mu. The resource may have changed while the
// effect ran (claimed, gone); each case says what that means.
func (m *Manager) applyEffects(results []EffectResult) {
	now := m.now()
	for _, res := range results {
		e := res.Effect
		delete(m.pending, e.Res.ID)
		r := m.st.Resources[e.Res.ID]
		switch e.Op {
		case "resume":
			if res.Err != nil {
				m.event("RESUME FAILED %s: %v", e.Res.ID, res.Err)
				continue
			}
			if r != nil {
				r.PausedAt, r.OrphanSince, r.Urgent = nil, nil, false
			}
			delete(m.connSeen, e.Res.ID)
			m.event("RESUMED %s (%s): %s", e.Res.ID, e.Res.Name, e.Why)
		case "pause":
			if res.Err != nil {
				m.event("PAUSE FAILED %s: %v; it will be stopped instead", e.Res.ID, res.Err)
				if r != nil {
					r.Urgent = true
				}
				continue
			}
			if r == nil {
				continue
			}
			t := now
			r.PausedAt = &t
			if r.LeaseID != "" || r.Pinned {
				// Claimed while it was being paused: thaw it again at once.
				Thaw := Effect{Op: "resume", Res: *r, Why: "claimed while it was being paused"}
				go func() { m.afterAsync(RunEffects([]Effect{Thaw})) }()
				continue
			}
			hold := minutes(m.cfg.PauseMinutes)
			m.event("PAUSED %s (%s, %.1f GB): %s. Nothing is lost; resume with: %s. Stopped at %s if still unclaimed.",
				r.ID, r.Name, r.MemMB/1024, e.Why, discover.HowToResume(r), now.Add(hold).Format("15:04"))
			m.notify("Paused "+r.Name, "Nothing is lost. Resume: "+discover.HowToResume(r)+". Stopped at "+now.Add(hold).Format("15:04")+".")
		case "stop":
			if res.Err != nil {
				m.event("STOP FAILED %s: %v", e.Res.ID, res.Err)
				continue
			}
			m.noteStopped(e.Res, now)
			m.recordStopped(e.Res, e.Why, e.By, now)
			m.noteGone(&e.Res, now)
			m.yieldTaken(e.Res.ID, now)
			if l := m.st.Leases[e.Res.LeaseID]; l != nil {
				l.Resources = remove(l.Resources, e.Res.ID)
			}
			delete(m.st.Resources, e.Res.ID)
			delete(m.st.Questions, e.Res.ID)
			m.activity.Forget(e.Res.ID)
			m.event("STOPPED %s (%s, %.1f GB): %s. Restore: agentrm restore %s", e.Res.ID, e.Res.Name, e.Res.MemMB/1024, e.Why, e.Res.ID)
			m.notify("Stopped "+e.Res.Name, "Freed ~"+gb(e.Res.MemMB)+". Restore: agentrm restore "+e.Res.ID)
		case "restore":
			if res.Err != nil {
				if e.Stopped != nil && !m.hasStopped(e.Stopped) {
					m.st.Stopped = append(m.st.Stopped, e.Stopped)
				}
				if l := m.st.Leases[e.Res.LeaseID]; l != nil && len(l.Resources) == 0 {
					m.dropLease(l, "the restore it was granted for failed")
				}
				delete(m.dockerOrigin, e.Res.ID)
				m.event("RESTORE FAILED %s: %v. It is still listed as stopped.", e.Res.ID, res.Err)
				continue
			}
			m.event("RESTORED %s (%s) for %s", e.Res.ID, e.Res.Name, e.By)
			m.kickSweep()
		case "restart":
			if res.Err != nil {
				m.event("RESTART FAILED %s: %v", e.Res.ID, res.Err)
				continue
			}
			m.event("RESTARTED %s (%s): %s", e.Res.ID, e.Res.Name, e.Why)
			m.notify("Restarted "+e.Res.Name, e.Why)
			m.kickSweep()
		}
	}
}

// afterAsync applies results of effects started outside a request.
func (m *Manager) afterAsync(results []EffectResult) {
	m.mu.Lock()
	m.applyEffects(results)
	m.mu.Unlock()
}

func (m *Manager) recordStopped(r model.Resource, why, by string, now time.Time) {
	if by == "" {
		by = "agentrm"
	}
	// A process AgentRM may start again: how it was started came with the resource, read
	// while it ran (keepRestartInfo). Failing that, only from the process with this PID and
	// start, never from whatever has the PID now.
	if r.PID > 0 && discover.Restartable(&r) {
		if len(r.RestartArgv) == 0 && m.infos != nil {
			if info := m.infos.getStarted(r.PID, r.ProcStart); len(info.Argv) > 0 {
				r.RestartArgv, r.RestartEnv = info.Argv, info.EnvList()
			}
		}
		if len(r.RestartArgv) > 0 && (r.Restart == "" || strings.HasPrefix(r.Restart, "re-run")) {
			r.Restart = "agentrm restore " + r.ID
		}
	} else {
		r.RestartArgv, r.RestartEnv = nil, nil
	}
	r.PausedAt, r.OrphanSince, r.LeaseID, r.Urgent = nil, nil, "", false
	m.st.Stopped = append(m.st.Stopped, &model.Stopped{Resource: r, StoppedAt: now, Why: why, By: by})
	if len(m.st.Stopped) > 50 {
		m.st.Stopped = m.st.Stopped[len(m.st.Stopped)-50:]
	}
}

func (m *Manager) hasStopped(s *model.Stopped) bool {
	for _, x := range m.st.Stopped {
		if x == s {
			return true
		}
	}
	return false
}

func gb(mb float64) string { return fmt.Sprintf("%.1f GB", mb/1024) }

// gateAuto is the last check on what AgentRM decided by itself, whichever code path decided it:
// in dry run (for that resource's auto-stop level) nothing is paused or stopped. Each decision
// site checks too; this makes a new one that forgets harmless. Must hold mu.
func (m *Manager) gateAuto(effects []Effect) []Effect {
	out := effects[:0]
	for _, e := range effects {
		if e.By == "agentrm" && (e.Op == "pause" || e.Op == "stop") {
			if why := m.failOpen(m.now()); why != "" {
				delete(m.pending, e.Res.ID)
				m.event("HELD automatic %s of %s: %s", e.Op, e.Res.ID, why)
				continue
			}
			if m.dryFor(&e.Res) {
				delete(m.pending, e.Res.ID)
				m.event("DRYRUN blocked an automatic %s of %s that a decision path let through", e.Op, e.Res.ID)
				continue
			}
		}
		out = append(out, e)
	}
	return out
}
