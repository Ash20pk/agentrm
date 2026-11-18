package manager

import (
	"fmt"
	"sync"
	"time"

	"agentrm/internal/discover"
	"agentrm/internal/model"
)

// Effect is an action on a resource decided under the lock and carried out outside it (D1).
type Effect struct {
	Op  string // pause, resume, stop, restore
	Res model.Resource
	Why string
	By  string // who asked: "agentrm" or a session
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
				m.event("RESTORE FAILED %s: %v", e.Res.ID, res.Err)
				continue
			}
			m.event("RESTORED %s (%s) for %s", e.Res.ID, e.Res.Name, e.By)
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
	r.PausedAt, r.OrphanSince, r.LeaseID, r.Urgent = nil, nil, "", false
	m.st.Stopped = append(m.st.Stopped, &model.Stopped{Resource: r, StoppedAt: now, Why: why, By: by})
	if len(m.st.Stopped) > 50 {
		m.st.Stopped = m.st.Stopped[len(m.st.Stopped)-50:]
	}
}

func gb(mb float64) string { return fmt.Sprintf("%.1f GB", mb/1024) }
