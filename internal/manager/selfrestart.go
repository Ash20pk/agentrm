package manager

// Things that come back by themselves once stopped: Docker Desktop's VM, which its engine
// starts again. They are not leased, not attributed to a session, and not restored: reclaiming
// one restarts it, and what it held above its idle size comes back to macOS.
//
// Each has its own trust level (opt-in, see trust.go). Until you turn that on, AgentRM only
// asks: once per episode it records what it would do, for you to review, and tells you. Only
// you can restart one by hand (agentrm stop); an agent never can. Nothing is restarted while
// anything uses it, and the check is made again right before (discover.RestartDockerEngine).

import (
	"fmt"
	"time"

	"agentrm/internal/discover"
	"agentrm/internal/model"
)

const (
	// dockerVMMinMB is the least a restart must give back to be worth asking about.
	dockerVMMinMB = 1024
	// restartCooldown: something AgentRM restarted is left alone this long after.
	restartCooldown = time.Hour
)

// restartMark is when AgentRM restarted something, and whether its idle size has been read
// since.
type restartMark struct {
	at      time.Time
	sampled bool
}

var dockerLeaseKinds = map[string]bool{model.KCluster: true, model.KCompose: true, model.KDockerBuild: true}

// restartBlocker says why r cannot be restarted now, or "". Must hold mu.
func (m *Manager) restartBlocker(r *model.Resource, now time.Time) string {
	if r.Kind == model.RKDockerVM {
		return m.dockerBusy(r, now)
	}
	return "it is not something AgentRM restarts"
}

// dockerBusy says what uses Docker now, or "". Anything that might: a container running (or
// last seen running, on an engine that is not answering), a lease of a Docker kind, a request
// waiting for one (a restart would only delay it, and the VM reuses its own memory for it), or
// the VM busy on its own (a build with no lease).
func (m *Manager) dockerBusy(r *model.Resource, now time.Time) string {
	if err := m.failed[discover.SrcDocker]; err != nil {
		return "Docker is not answering, so what runs in it cannot be seen"
	}
	for _, x := range m.st.Resources {
		if x.Source == discover.SrcDocker {
			return fmt.Sprintf("%s runs in it", x.ID)
		}
	}
	for _, l := range m.st.Leases {
		if dockerLeaseKinds[l.Kind] {
			return fmt.Sprintf("lease %s (%s) uses Docker", l.ID, l.Kind)
		}
	}
	for _, w := range m.liveWaiters(now) {
		if dockerLeaseKinds[w.Kind] {
			return fmt.Sprintf("a %s request is waiting for it", w.Kind)
		}
	}
	if r.CPU > 10 {
		return fmt.Sprintf("the VM is busy (%.0f%% CPU)", r.CPU)
	}
	return ""
}

// checkSelfRestarting decides about one thing that comes back by itself: nothing while it is
// used, held, pinned or exempt; once it has been unused for the grace period, ask you (its level
// off) or restart it (its level on). Must hold mu.
func (m *Manager) checkSelfRestarting(r *model.Resource, now time.Time) *Effect {
	busy := m.restartBlocker(r, now)
	if busy == "" && r.MemMB < dockerVMMinMB {
		busy = "too little to give back"
	}
	if mark := m.restartedAt[r.ID]; mark != nil && now.Sub(mark.at) < restartCooldown {
		busy = "restarted " + now.Sub(mark.at).Round(time.Minute).String() + " ago"
	}
	if busy != "" || r.Pinned || m.neverStop(r) {
		r.OrphanSince, r.Urgent = nil, false
		return nil
	}
	if r.OrphanSince == nil {
		t := now
		r.OrphanSince = &t
		return nil
	}
	idle := now.Sub(*r.OrphanSince)
	if idle < minutes(m.cfg.GraceMinutes) && !r.Urgent {
		return nil
	}
	why := fmt.Sprintf("nothing has used Docker for %s, and its VM holds %.1f GB more than it needs idle",
		idle.Round(time.Minute), r.MemMB/1024)
	if r.Urgent {
		why += ", and a request was waiting for memory"
	}
	if m.dryFor(r) {
		m.decideOnce("stop", r, why)
		if key := "ask:" + r.ID + ":" + r.OrphanSince.Format(time.RFC3339); !m.wouldDecided[key] {
			m.wouldDecided[key] = true
			m.event("ASKYOU %s: %s. Restarting Docker Desktop gives it back; run agentrm stop %s, or restart it from the app.",
				r.ID, why, r.ID)
			m.notify("Docker's VM holds "+gb(r.MemMB)+" it does not need",
				"No container has run for "+idle.Round(time.Minute).String()+". Restart Docker Desktop to give it back (agentrm stop "+r.ID+").")
		}
		return nil
	}
	m.decide("stop", r, why)
	m.pending[r.ID] = "stop"
	return &Effect{Op: "stop", Res: *r, Why: why, By: "agentrm"}
}

// restartedSelf records that something which comes back by itself was restarted: it is not
// listed as stopped (there is nothing to restore), and it is left alone for a while. Must hold
// mu.
func (m *Manager) restartedSelf(r model.Resource, why string, now time.Time) {
	m.restartedAt[r.ID] = &restartMark{at: now}
	m.event("RESTARTED %s (%s, ~%.1f GB back): %s. It comes back by itself.", r.ID, r.Name, r.MemMB/1024, why)
	m.notify("Restarted "+r.Name, "Gave back ~"+gb(r.MemMB)+". It comes back by itself.")
}
