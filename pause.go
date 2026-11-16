package main

// Pause before stop.
//
// Stopping the wrong thing costs something: a dev server loses what it held in memory, a
// cluster loses a test run. So an unclaimed resource is first frozen (SIGSTOP, docker pause),
// which loses nothing and is undone in an instant, and only stopped if it is still unclaimed
// a while later. A frozen resource is thawed the moment anyone claims it, pins it, or runs a
// command that uses it.
//
// Freezing gives back CPU at once but not memory (the kernel can then compress or swap the
// frozen pages without them being touched again). So when a request is blocked waiting for
// memory, the freeze is skipped and the resource is stopped directly.

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

func canPause(r *Resource) bool {
	return r.Kind != RKSimulator // simctl has no freeze
}

func pause(r *Resource) error {
	switch r.Kind {
	case RKCluster, RKContainers:
		_, err := run("docker", append([]string{"pause"}, r.Containers...)...)
		return err
	case RKGradle, RKServer:
		_, err := run("kill", "-STOP", strconv.Itoa(r.PID))
		return err
	}
	return fmt.Errorf("%s cannot be paused", r.Kind)
}

func resume(r *Resource) error {
	switch r.Kind {
	case RKCluster, RKContainers:
		// Unpause one by one: some may already be running.
		var firstErr error
		for _, c := range r.Containers {
			if _, err := run("docker", "unpause", c); err != nil && firstErr == nil {
				if state, _ := run("docker", "inspect", "-f", "{{.State.Paused}}", c); strings.TrimSpace(state) == "true" {
					firstErr = err
				}
			}
		}
		return firstErr
	case RKGradle, RKServer:
		_, err := run("kill", "-CONT", strconv.Itoa(r.PID))
		return err
	}
	return nil
}

func howToResume(r *Resource) string { return "agentrm resume " + r.ID }

// thaw resumes a paused resource and restarts its orphan clock.
func (d *Daemon) thaw(r *Resource, why string) error {
	if r.PausedAt == nil {
		return nil
	}
	if err := resume(r); err != nil {
		d.event("RESUME FAILED %s: %v", r.ID, err)
		return err
	}
	r.PausedAt, r.OrphanSince, r.Urgent = nil, nil, false
	d.event("RESUMED %s (%s): %s", r.ID, r.Name, why)
	return nil
}

func (d *Daemon) resumeNow(req map[string]any) (any, error) {
	rid := str(req, "resource_id")
	var n int
	for id, r := range d.st.Resources {
		if (rid == "--all" || id == rid) && r.PausedAt != nil {
			if err := d.thaw(r, "resumed by "+str(req, "session")); err != nil {
				return nil, err
			}
			n++
		}
	}
	if n == 0 && rid != "--all" {
		if d.st.Resources[rid] == nil {
			return nil, fmt.Errorf("no running resource %q", rid)
		}
		return nil, fmt.Errorf("%s is not paused", rid)
	}
	return map[string]int{"resumed": n}, nil
}

// checkOrphans asks about unclaimed resources, then pauses, then stops them.
func (d *Daemon) checkOrphans(now time.Time) {
	grace := time.Duration(d.cfg.GraceMinutes) * time.Minute
	hold := time.Duration(d.cfg.PauseMinutes) * time.Minute
	blocked := len(d.liveWaiters(now)) > 0
	for _, r := range d.st.Resources {
		if r.LeaseID != "" || r.Pinned || d.neverStop(r) {
			if r.PausedAt != nil {
				_ = d.thaw(r, "it is held or pinned now")
			}
			r.OrphanSince, r.Urgent = nil, false
			delete(d.st.Questions, r.ID)
			continue
		}
		if r.OrphanSince == nil {
			t := now
			r.OrphanSince = &t
			next := "stopped"
			if d.cfg.PauseMinutes > 0 && canPause(r) {
				next = fmt.Sprintf("paused, and stopped %d min later,", d.cfg.PauseMinutes)
			}
			d.st.Questions[r.ID] = &Question{ResourceID: r.ID, Created: now,
				Delivered: map[string]bool{}, Answers: map[string]string{},
				Text: fmt.Sprintf("[agentrm] %s (%s, %s, %.1f GB) is running with no lease. If you are using it, run: "+
					"agentrm claim %s --purpose \"<why>\"   — if not, run: agentrm disown %s. "+
					"Unclaimed, it is %s at %s.", r.ID, r.Name, r.Detail, r.MemMB/1024, r.ID, r.ID, next,
					now.Add(grace).Format("15:04"))}
			d.event("ORPHAN %s (%s, %.1f GB): asking active sessions; %s at %s",
				r.ID, r.Name, r.MemMB/1024, next, now.Add(grace).Format("15:04"))
			notify("Unclaimed: "+r.Name, fmt.Sprintf("%.1f GB. It is %s at %s unless someone claims it.",
				r.MemMB/1024, next, now.Add(grace).Format("15:04")))
			continue
		}
		if now.Sub(*r.OrphanSince) < grace || d.cfg.DryRun {
			continue
		}
		// Freeze first, unless memory is needed now or freezing is not possible.
		direct := r.Urgent || blocked || d.cfg.PauseMinutes <= 0 || !canPause(r)
		if r.PausedAt == nil && !direct {
			if err := pause(r); err != nil {
				d.event("PAUSE FAILED %s: %v; it will be stopped instead", r.ID, err)
				r.Urgent = true
				continue
			}
			t := now
			r.PausedAt = &t
			d.event("PAUSED %s (%s, %.1f GB), unclaimed for %s. Nothing is lost; resume with: %s. Stopped at %s if still unclaimed.",
				r.ID, r.Name, r.MemMB/1024, grace, howToResume(r), now.Add(hold).Format("15:04"))
			notify("Paused "+r.Name, "Nothing is lost. Resume: "+howToResume(r)+". Stopped at "+now.Add(hold).Format("15:04")+".")
			continue
		}
		if r.PausedAt != nil && !direct && now.Sub(*r.PausedAt) < hold {
			continue
		}
		if err := stop(r); err != nil {
			d.event("STOP FAILED %s: %v", r.ID, err)
			continue
		}
		why := fmt.Sprintf("unclaimed for %s", now.Sub(*r.OrphanSince).Round(time.Minute))
		if r.Urgent || blocked {
			why += ", and a request was waiting for memory"
		}
		d.event("STOPPED %s (%s, %.1f GB), %s. Restart: %s", r.ID, r.Name, r.MemMB/1024, why, howToRestart(r))
		notify("Stopped "+r.Name, fmt.Sprintf("Freed ~%.1f GB. Restart: %s", r.MemMB/1024, howToRestart(r)))
		delete(d.st.Resources, r.ID)
		delete(d.st.Questions, r.ID)
	}
}
