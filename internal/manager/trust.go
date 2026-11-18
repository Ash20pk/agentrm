package manager

// Graduated trust. Auto-stop is not one switch: it unlocks level by level, in order of how
// strong the evidence behind each kind of decision is, and each level only when its own
// reviewed record says so:
//
//  1. owner_gone: what an ended session or a finished task left, unused since;
//  2. unclaimed: things nobody is known to own;
//  3. idle_lease: things whose lease was released because their holder let them sit idle and
//     did not answer.
//
// A level may be turned on once its decisions have been reviewed (at least 5, over at least 7
// days, under 2% wrong) and the level before it is on. A stop taken under a level and later
// marked wrong turns that level off again at once.

import (
	"fmt"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/model"
)

var trustLevels = []struct{ cat, title, detail string }{
	{"owner_gone", "What ended sessions left", "an agent exited or finished its task, and nothing has used it since"},
	{"unclaimed", "Unclaimed things", "running with no lease, and nobody is known to own it"},
	{"idle_lease", "Idle leases", "its holder let it sit idle and did not answer when asked"},
}

// categoryOf is the auto-stop level a decision about r belongs to.
func (m *Manager) categoryOf(r *model.Resource) string {
	switch {
	case r == nil:
		return ""
	case m.ownerGone(r) != nil:
		return "owner_gone"
	case r.ReleasedIdle:
		return "idle_lease"
	}
	return "unclaimed"
}

// dryFor says whether decisions about r are only reported (dry run for its level).
func (m *Manager) dryFor(r *model.Resource) bool {
	if !m.cfg.DryRun {
		return false
	}
	return !m.cfg.AutoStop[m.categoryOf(r)]
}

// trustLevels is the evidence for each level, for status and the app.
func (m *Manager) trustLevels(now time.Time) []api.TrustLevel {
	var out []api.TrustLevel
	prevOn := true
	for _, lv := range trustLevels {
		t := api.TrustLevel{Category: lv.cat, Title: lv.title, Detail: lv.detail, On: !m.cfg.DryRun || m.cfg.AutoStop[lv.cat]}
		var first time.Time
		for _, d := range m.st.Decisions {
			if d.Category != lv.cat || !collecting(d) {
				continue
			}
			t.Decided++
			if first.IsZero() || d.Time.Before(first) {
				first = d.Time
			}
			if d.Verdict != "" {
				t.Reviewed++
				if d.Verdict == "wrong" {
					t.Wrong++
				}
			}
		}
		if !first.IsZero() {
			t.Days = int(now.Sub(first).Hours() / 24)
		}
		rate := 1.0
		if t.Reviewed > 0 {
			rate = float64(t.Wrong) / float64(t.Reviewed)
		}
		t.Eligible = t.Reviewed >= 5 && t.Days >= 7 && rate < 0.02
		if !prevOn {
			t.Blocked = "turn on the level above first"
		}
		switch {
		case t.Decided == 0:
			t.Evidence = "No decisions of this kind yet."
		case t.Reviewed == 0:
			t.Evidence = fmt.Sprintf("%s in %s; none reviewed yet.", plural(t.Decided, "decision", "decisions"), plural(t.Days, "day", "days"))
		default:
			t.Evidence = fmt.Sprintf("%s in %s; %d reviewed, %d wrong (%.0f%%).", plural(t.Decided, "decision", "decisions"),
				plural(t.Days, "day", "days"), t.Reviewed, t.Wrong, 100*rate)
		}
		prevOn = t.On
		out = append(out, t)
	}
	return out
}

// setAutoStop applies requested level changes, in order: a level is turned on only if the one
// before it is on. It returns what changed.
func (m *Manager) applyAutoStop(cfg *model.Config, want map[string]bool) ([]string, error) {
	if cfg.AutoStop == nil {
		cfg.AutoStop = map[string]bool{}
	}
	var changes []string
	prevOn := true
	for _, lv := range trustLevels {
		on, asked := want[lv.cat]
		if asked {
			if on && !prevOn {
				return nil, fmt.Errorf("turn on %q first", trustLevelTitle(prevCat(lv.cat)))
			}
			if cfg.AutoStop[lv.cat] != on {
				cfg.AutoStop[lv.cat] = on
				changes = append(changes, fmt.Sprintf("auto_stop.%s=%v", lv.cat, on))
			}
		}
		prevOn = !cfg.DryRun || cfg.AutoStop[lv.cat]
	}
	for k := range want {
		if trustLevelTitle(k) == "" {
			return nil, fmt.Errorf("no auto-stop level %q (owner_gone, unclaimed, idle_lease)", k)
		}
	}
	// Turning a level off turns off the ones after it.
	off := false
	for _, lv := range trustLevels {
		if off && cfg.AutoStop[lv.cat] {
			cfg.AutoStop[lv.cat] = false
			changes = append(changes, fmt.Sprintf("auto_stop.%s=false", lv.cat))
		}
		off = off || !cfg.AutoStop[lv.cat]
	}
	return changes, nil
}

func trustLevelTitle(cat string) string {
	for _, lv := range trustLevels {
		if lv.cat == cat {
			return lv.title
		}
	}
	return ""
}

func prevCat(cat string) string {
	for i, lv := range trustLevels {
		if lv.cat == cat && i > 0 {
			return trustLevels[i-1].cat
		}
	}
	return ""
}

// relock turns a level off when a stop taken under it is marked wrong.
func (m *Manager) relock(d *model.Decision) {
	if d.DryRun || d.Verdict != "wrong" || d.Category == "" || m.cfg.DryRun == false && len(m.cfg.AutoStop) == 0 {
		return
	}
	if !m.cfg.AutoStop[d.Category] {
		return
	}
	cfg := m.cfg
	cfg.AutoStop = map[string]bool{}
	for k, v := range m.cfg.AutoStop {
		cfg.AutoStop[k] = v
	}
	changes, _ := m.applyAutoStop(&cfg, map[string]bool{d.Category: false})
	m.cfg, m.cfgDirty = cfg, true
	m.event("LOCKED auto-stop for %q: you marked its %s of %s wrong (%v). It is back to asking and reporting.",
		trustLevelTitle(d.Category), d.Action, d.Resource, changes)
	m.notify("Auto-stop paused for "+trustLevelTitle(d.Category), "You marked one of its stops wrong, so it only reports again.")
}

// collect stops now what ended sessions and finished tasks left, at your request (the app's
// one button on first run). Things that are never stopped automatically stay.
func (m *Manager) collect(c api.Caller, r api.Req) (any, []Effect, error) {
	var targets []*model.Resource
	mb := 0.0
	for _, res := range m.st.Resources {
		if res.OwnerGoneAt == nil || res.LeaseID != "" || res.Pinned || res.AskOnly || m.neverStop(res) ||
			res.StartedBy == "you" || m.pending[res.ID] != "" {
			continue
		}
		targets = append(targets, res)
		mb += res.MemMB
	}
	if len(targets) == 0 {
		return &api.CountResp{N: 0, Note: "nothing left behind to collect"}, nil, nil
	}
	if err := m.privileged(c, fmt.Sprintf("collect %d thing(s) ended sessions left (%.1f GB)", len(targets), mb/1024)); err != nil {
		return nil, nil, err
	}
	var effects []Effect
	for _, res := range targets {
		why := "collected at your request: its session had ended"
		m.decide("stop", res, why)
		m.st.Decisions[len(m.st.Decisions)-1].DryRun = false
		m.pending[res.ID] = "stop"
		effects = append(effects, Effect{Op: "stop", Res: *res, Why: why, By: "you"})
	}
	m.event("COLLECT %d thing(s) (%.1f GB) left by ended sessions, at your request", len(targets), mb/1024)
	return &api.CountResp{N: len(targets), Note: fmt.Sprintf("%.1f GB", mb/1024)}, effects, nil
}
