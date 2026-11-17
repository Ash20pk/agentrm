package manager

// Unclaimed resources: ask everyone, then pause (losing nothing), then stop. A resource nobody
// claims is asked about at once; after the grace period it is frozen, and after the pause
// period stopped. If a request is waiting for memory the freeze is skipped, because a frozen
// process still holds its memory. VMs are never stopped automatically.
//
// Every pause and stop decision, taken or (in dry run) not, is logged for review.

import (
	"fmt"
	"sort"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/discover"
	"agentrm/internal/model"
	"agentrm/internal/trace"
)

// checkOrphans asks about, pauses and stops unclaimed resources. Must hold mu; the returned
// effects are run by the caller after unlocking.
func (m *Manager) checkOrphans(now time.Time) []Effect {
	blocked := len(m.liveWaiters(now)) > 0
	var effects []Effect
	for _, r := range m.st.Resources {
		if m.pending[r.ID] != "" {
			continue
		}
		if r.LeaseID != "" || r.Pinned || m.neverStop(r) {
			if r.PausedAt != nil {
				effects = append(effects, m.thawEffect(r, "it is held or pinned now"))
			}
			r.OrphanSince, r.Urgent = nil, false
			delete(m.st.Questions, r.ID)
			continue
		}
		grace, hold, reason, ownerGone := m.collectTiming(r, now, blocked)
		if r.OrphanSince == nil {
			t := now
			r.OrphanSince = &t
			next := "stopped"
			switch {
			case !discover.CanAutoStop(r):
				next = "paused (VMs are never stopped automatically)"
			case m.cfg.PauseMinutes > 0 && discover.CanPause(r):
				next = fmt.Sprintf("paused, and stopped %d min later,", int(hold.Minutes()))
			}
			m.st.Questions[r.ID] = &model.Question{ResourceID: r.ID, Created: now,
				Delivered: map[string]bool{}, Answers: map[string]string{},
				Text: fmt.Sprintf("[agentrm] %s (%s, %s, %.1f GB) is running with no lease. If you are using it, run: "+
					"agentrm claim %s --purpose \"<why>\"   — if not, run: agentrm disown %s. "+
					"Unclaimed, it is %s at %s.", r.ID, r.Name, r.Detail, r.MemMB/1024, r.ID, r.ID, next,
					now.Add(grace).Format("15:04"))}
			if ownerGone {
				// The session's end already told the user, once for everything it left.
				m.event("ORPHAN %s (%s, %.1f GB): %s; %s at %s", r.ID, r.Name, r.MemMB/1024, reason, next, now.Add(grace).Format("15:04"))
				continue
			}
			m.event("ORPHAN %s (%s, %.1f GB): asking active sessions; %s at %s",
				r.ID, r.Name, r.MemMB/1024, next, now.Add(grace).Format("15:04"))
			m.notify("Unclaimed: "+r.Name, fmt.Sprintf("%.1f GB. It is %s at %s unless someone claims it.",
				r.MemMB/1024, next, now.Add(grace).Format("15:04")))
			continue
		}
		if now.Sub(*r.OrphanSince) < grace {
			continue
		}
		if r.StartedBy == "you" {
			// What you started is yours to stop: AgentRM asks once, and never pauses or stops it.
			if !m.wouldDecided["ask:"+r.ID] {
				m.wouldDecided["ask:"+r.ID] = true
				m.event("ASKYOU %s (%s, %.1f GB), which you started, has been unclaimed for %s. AgentRM leaves it to you.",
					r.ID, r.Name, r.MemMB/1024, now.Sub(*r.OrphanSince).Round(time.Minute))
				m.notify("Still using "+r.Name+"?", fmt.Sprintf("You started it; it holds %s. AgentRM will not stop it.", gb(r.MemMB)))
			}
			continue
		}
		direct := r.Urgent || blocked || m.cfg.PauseMinutes <= 0 || !discover.CanPause(r)
		why := fmt.Sprintf("unclaimed for %s", now.Sub(*r.OrphanSince).Round(time.Minute))
		if reason != "" {
			why = reason
		}
		if r.Urgent || blocked {
			why += ", and a request was waiting for memory"
		}
		switch {
		case r.PausedAt == nil && !direct:
			if m.dryFor(r) {
				m.decideOnce("pause", r, why)
				continue
			}
			m.decide("pause", r, why)
			m.pending[r.ID] = "pause"
			effects = append(effects, Effect{Op: "pause", Res: *r, Why: why, By: "agentrm"})
		case r.PausedAt != nil && !direct && now.Sub(*r.PausedAt) < hold:
			continue
		case !discover.CanAutoStop(r):
			if r.PausedAt == nil && discover.CanPause(r) && !m.dryFor(r) {
				m.decide("pause", r, why)
				m.pending[r.ID] = "pause"
				effects = append(effects, Effect{Op: "pause", Res: *r, Why: why, By: "agentrm"})
			} else if !m.wouldDecided["ask:"+r.ID] {
				m.wouldDecided["ask:"+r.ID] = true
				m.event("ASKYOU %s (%s, %.1f GB) is unclaimed, and %s; stop it yourself if you are done with it.",
					r.ID, r.Name, r.MemMB/1024, discover.NoAutoStopWhy(r))
				m.notify("Unclaimed: "+r.Name, "AgentRM will not stop it. Stop it yourself if you are done.")
			}
		default:
			if r.PausedAt != nil {
				why += fmt.Sprintf(", then paused for %s", now.Sub(*r.PausedAt).Round(time.Minute))
			}
			if m.dryFor(r) {
				m.decideOnce("stop", r, why)
				continue
			}
			m.decide("stop", r, why)
			m.pending[r.ID] = "stop"
			effects = append(effects, Effect{Op: "stop", Res: *r, Why: why, By: "agentrm"})
		}
	}
	return effects
}

// decide logs a pause, stop or reclaim decision for review.
func (m *Manager) decide(action string, r *model.Resource, why string) {
	d := &model.Decision{ID: newID(), Time: m.now(), Action: action, Why: why, DryRun: m.cfg.DryRun}
	if r != nil {
		d.Category, d.DryRun = m.categoryOf(r), m.dryFor(r)
	}
	if r != nil {
		d.Resource, d.Name, d.MemMB = r.ID, r.Name, r.MemMB
	}
	m.st.Decisions = append(m.st.Decisions, d)
	if m.onDecide != nil {
		m.onDecide(d)
	}
	if m.rec != nil {
		m.rec.Write(trace.Frame{T: d.Time, Decision: d})
	}
	if len(m.st.Decisions) > 500 {
		m.st.Decisions = m.st.Decisions[len(m.st.Decisions)-500:]
	}
	if d.DryRun && r != nil {
		m.event("WOULD %s %s (%s, %.1f GB): %s. Dry run, so nothing was done.", action, r.ID, r.Name, r.MemMB/1024, why)
	}
}

// decideOnce logs a dry-run decision once per orphan episode.
func (m *Manager) decideOnce(action string, r *model.Resource, why string) {
	key := action + ":" + r.ID + ":" + r.OrphanSince.Format(time.RFC3339)
	if m.wouldDecided[key] {
		return
	}
	m.wouldDecided[key] = true
	m.decide(action, r, why)
}

func (m *Manager) resumeNow(c api.Caller, r api.Req) (any, []Effect, error) {
	var effects []Effect
	for id, res := range m.st.Resources {
		if (r.All || id == r.ResourceID) && res.PausedAt != nil && m.pending[id] == "" {
			effects = append(effects, m.thawEffect(res, "resumed by "+c.Session))
		}
	}
	if len(effects) == 0 && !r.All {
		if m.st.Resources[r.ResourceID] == nil {
			return nil, nil, fmt.Errorf("no running resource %q", r.ResourceID)
		}
		return nil, nil, fmt.Errorf("%s is not paused", r.ResourceID)
	}
	return &api.CountResp{N: len(effects)}, effects, nil
}

// review lists decisions for the user to mark, or records a mark. Only a person may mark:
// the marks are the calibration data for when auto-stop is safe to recommend.
func (m *Manager) review(c api.Caller, r api.Req) (any, []Effect, error) {
	if r.DecisionID != "" {
		if !c.Human {
			return nil, nil, fmt.Errorf("only a person can review decisions")
		}
		if r.Verdict != "right" && r.Verdict != "wrong" {
			return nil, nil, fmt.Errorf("verdict must be right or wrong")
		}
		for _, d := range m.st.Decisions {
			if d.ID == r.DecisionID {
				d.Verdict, d.Note = r.Verdict, r.Note
				m.event("REVIEW %s %s %s: %s", d.ID, d.Action, d.Resource, r.Verdict)
				m.relock(d)
				return m.reviewSummary(r.All), nil, nil
			}
		}
		return nil, nil, fmt.Errorf("no decision %s", r.DecisionID)
	}
	return m.reviewSummary(r.All), nil, nil
}

func (m *Manager) reviewSummary(all bool) *api.ReviewResp {
	out := &api.ReviewResp{FalseRate: -1}
	for _, d := range m.st.Decisions {
		if d.Verdict != "" && d.Action != "reclaim" {
			out.Reviewed++
			if d.Verdict == "wrong" {
				out.Wrong++
			}
		}
		if all || d.Verdict == "" {
			out.Decisions = append(out.Decisions, d)
		}
	}
	sort.Slice(out.Decisions, func(i, j int) bool { return out.Decisions[i].Time.After(out.Decisions[j].Time) })
	if out.Reviewed > 0 {
		out.FalseRate = float64(out.Wrong) / float64(out.Reviewed)
	}
	return out
}
