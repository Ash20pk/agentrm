package manager

// Making room ahead. A request that does not fit waits while reclaim runs, so the build sits
// idle while things stop. When the command stream says a session's next heavy step is likely
// (predict.go: per project, which kind usually follows the one just run), and that step would
// not fit, AgentRM starts on the room before the request comes:
//
//   - every confident prediction is scored in the accuracy ledger (predict:<kind>): a hit when
//     the session runs that kind within prepareWindow, a miss when it does not;
//   - only when predictions of that kind have come true often enough (prepareMinHits of at
//     least prepareMinTries) is anything done; until then what would have been done is logged;
//   - what is done is reversible only: unclaimed things nobody has used for a while are paused,
//     the cheapest first (left by an ended session, then idle longest), never stopped;
//   - a pause made on a prediction is undone when the prediction is settled: the request did
//     not come within the window, or it came and its lease has ended (at most prepareHitHold);
//   - it goes through the same plumbing as every automatic pause: dry run and the trust levels
//     (dryFor), the fail-open rules (failOpen, and gateAuto as the last check), never-stop,
//     pins, things you started, and nothing held by a live lease or last seen on a source
//     that no longer answers (Unseen).
//
// A pause made ahead never leads to a stop by itself: checkOrphans leaves a prepared pause
// alone unless a request is really waiting, which is when it would take anything unclaimed
// that long anyway.

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"agentrm/internal/discover"
	"agentrm/internal/model"
)

const (
	prepareWindow   = 10 * time.Minute // a predicted request that has not come by then is a miss
	prepareHitHold  = 30 * time.Minute // the longest room made ahead is kept for a request that came
	prepareMinTries = 5                // predictions of a kind scored before any is acted on
	prepareMinHits  = 0.7              // and the share of them that must have come true
)

// prep is one open prediction: session will likely ask for kind soon.
type prep struct {
	id      string
	session string
	kind    string
	after   string // the kind just run, that it usually follows
	prob    float64
	n       int
	needMB  float64
	at      time.Time
	until   time.Time  // the end of the window
	hitAt   *time.Time // when the request came
	acted   bool       // room was made, or would have been: once per prediction
	done    string     // settled, and why: what was paused for it is resumed
}

// notePrediction opens a prediction to be scored, unless one for this session and kind is
// already waiting. Must hold mu.
func (m *Manager) notePrediction(session, kind, after string, prob float64, n int, needMB float64, now time.Time) {
	for _, p := range m.preps {
		if p.session == session && p.kind == kind && p.hitAt == nil && p.done == "" && now.Before(p.until) {
			return
		}
	}
	id := newID()
	m.preps[id] = &prep{id: id, session: session, kind: kind, after: after, prob: prob, n: n, needMB: needMB,
		at: now, until: now.Add(prepareWindow)}
}

// predictionArrived scores the open predictions of this session and kind as hits: the request
// came. Must hold mu.
func (m *Manager) predictionArrived(session, kind string, now time.Time) {
	for _, p := range m.preps {
		if p.session != session || p.kind != kind || p.hitAt != nil || p.done != "" || !now.Before(p.until) {
			continue
		}
		t := now
		p.hitAt = &t
		m.score("predict:"+kind, 1, 1, now)
		hold := now.Add(prepareHitHold)
		for _, r := range m.st.Resources {
			if r.PreparedFor == p.id {
				r.PreparedUntil = &hold
				m.event("PREDICTED %s asked for %s %s after it was predicted; %s stays paused until that work is done (at most until %s)",
					short(session), kind, now.Sub(p.at).Round(time.Second), r.ID, hold.Format("15:04"))
			}
		}
	}
}

// predictTrust says whether predictions of kind have come true often enough to act on, and
// their record in words.
func (m *Manager) predictTrust(kind string) (bool, string) {
	a := m.st.Accuracy["predict:"+kind]
	if a == nil || a.N == 0 {
		return false, "no prediction of it has been scored yet"
	}
	rate := a.Actual / float64(a.N)
	rec := fmt.Sprintf("predictions of it came %d/%d (%.0f%%)", int(a.Actual), a.N, 100*rate)
	return a.N >= prepareMinTries && rate >= prepareMinHits, rec
}

// tendPrepared settles predictions whose time is up, undoes what was paused for them, and
// makes room for open ones that would not fit. Called on each full sweep. Must hold mu; the
// returned effects are run after unlocking.
func (m *Manager) tendPrepared(now time.Time) []Effect {
	for _, p := range m.preps {
		switch {
		case p.done != "":
		case p.hitAt == nil && !now.Before(p.until):
			m.score("predict:"+p.kind, 1, 0, now)
			p.done = fmt.Sprintf("%s did not ask for %s within %d min of the prediction", short(p.session), p.kind, int(prepareWindow.Minutes()))
		case p.hitAt != nil && now.Sub(*p.hitAt) >= prepareHitHold:
			p.done = fmt.Sprintf("the %s it was paused for came %d min ago, the longest room made ahead is kept", p.kind, int(prepareHitHold.Minutes()))
		case p.hitAt != nil && m.findLease(p.session, p.kind) == nil && !m.isWaiting(p.session, p.kind):
			p.done = fmt.Sprintf("the %s it was paused for is done", p.kind)
		}
	}
	var effects []Effect
	for _, r := range m.st.Resources {
		if r.PreparedFor == "" {
			continue
		}
		p := m.preps[r.PreparedFor]
		if p != nil && p.done == "" {
			continue
		}
		if r.PausedAt == nil {
			r.PreparedFor, r.PreparedUntil = "", nil
			continue
		}
		if m.pending[r.ID] != "" {
			continue
		}
		why := "the prediction it was paused for is no longer tracked"
		if p != nil {
			why = p.done
		}
		m.pending[r.ID] = "resume"
		effects = append(effects, Effect{Op: "resume", Res: *r, Why: why, By: "agentrm", Prepared: r.PreparedFor})
	}
	open := make([]*prep, 0, len(m.preps))
	for id, p := range m.preps {
		if p.done != "" {
			delete(m.preps, id)
			continue
		}
		open = append(open, p)
	}
	sort.Slice(open, func(i, j int) bool { return open[i].at.Before(open[j].at) })
	for _, p := range open {
		if !p.acted && p.hitAt == nil {
			effects = append(effects, m.makeRoom(p, now)...)
		}
	}
	return effects
}

// isWaiting says whether session has a request of kind in the queue.
func (m *Manager) isWaiting(session, kind string) bool {
	for _, w := range m.waiters {
		if w.Session == session && w.Kind == kind {
			return true
		}
	}
	return false
}

// makeRoom pauses, ahead of a predicted request, enough unclaimed idle things to cover what it
// would be short of, if predictions of its kind have earned it. Must hold mu.
func (m *Manager) makeRoom(p *prep, now time.Time) []Effect {
	shortMB := p.needMB - m.headroomMB()
	if shortMB <= 0 {
		return nil // it fits: nothing to do, unless that changes before it comes
	}
	if why := m.failOpen(now); why != "" {
		return nil // nothing is paused automatically while AgentRM lets everything through
	}
	var pick []*model.Resource
	got := 0.0
	for _, r := range m.prepareCandidates(p.session, now) {
		if got >= shortMB {
			break
		}
		pick = append(pick, r)
		got += r.MemMB
	}
	if len(pick) == 0 {
		return nil
	}
	p.acted = true
	names := make([]string, len(pick))
	for i, r := range pick {
		names[i] = fmt.Sprintf("%s (%.1f GB)", r.ID, r.MemMB/1024)
	}
	ok, record := m.predictTrust(p.kind)
	if !ok {
		m.event("PREDICT %s will likely ask for %s (%.0f%% of %d times after %s) and would be %.1f GB short; "+
			"AgentRM would pause %s ahead of it, but %s (it acts from %.0f%% of at least %d), so nothing was paused",
			short(p.session), p.kind, 100*p.prob, p.n, orDash(p.after), shortMB/1024, strings.Join(names, ", "),
			record, 100*prepareMinHits, prepareMinTries)
		return nil
	}
	why := fmt.Sprintf("paused ahead of %s's likely %s request, %.1f GB short (%s); resumed at %s if it does not come",
		short(p.session), p.kind, shortMB/1024, record, p.until.Format("15:04"))
	var effects []Effect
	for _, r := range pick {
		m.decide("prepare", r, why)
		if m.dryFor(r) {
			continue // logged as what it would have done
		}
		m.pending[r.ID] = "pause"
		effects = append(effects, Effect{Op: "pause", Res: *r, Why: why, By: "agentrm", Prepared: p.id})
	}
	return effects
}

// prepareCandidates is what may be paused ahead of a prediction for session, cheapest first:
// unclaimed (already asked about, held by no lease), idle for the pressure idle period, and
// pausable. Never what you started, what is pinned or exempt, what its source no longer shows,
// nor anything of the predicting session itself (its next step may need it). Must hold mu.
func (m *Manager) prepareCandidates(session string, now time.Time) []*model.Resource {
	idle := minutes(max(1, m.cfg.PressureIdleMinutes))
	var out []*model.Resource
	for _, r := range m.st.Resources {
		if r.LeaseID != "" || r.Unseen != nil || r.Pinned || r.AskOnly || m.neverStop(r) || !discover.CanPause(r) ||
			r.StartedBy == "you" || r.StartedBy == session || r.Owner == session ||
			r.PausedAt != nil || m.pending[r.ID] != "" || r.OrphanSince == nil {
			continue
		}
		last := r.LastActive
		if last.IsZero() {
			last = r.FirstSeen
		}
		if now.Sub(last) < idle {
			continue
		}
		out = append(out, r)
	}
	lastOf := func(r *model.Resource) time.Time {
		if r.LastActive.IsZero() {
			return r.FirstSeen
		}
		return r.LastActive
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if ga, gb := m.ownerGone(a) != nil, m.ownerGone(b) != nil; ga != gb {
			return ga
		}
		if la, lb := lastOf(a), lastOf(b); !la.Equal(lb) {
			return la.Before(lb)
		}
		if a.MemMB != b.MemMB {
			return a.MemMB > b.MemMB
		}
		return a.ID < b.ID
	})
	return out
}

// prepared records a pause made ahead of a prediction. Must hold mu.
func (m *Manager) prepared(r *model.Resource, e Effect, now time.Time) {
	r.PreparedFor = e.Prepared
	p := m.preps[e.Prepared]
	if p == nil || p.done != "" {
		// Settled while it was being paused: the next sweep resumes it.
		t := now
		r.PreparedUntil = &t
		return
	}
	until := p.until
	if p.hitAt != nil {
		until = p.hitAt.Add(prepareHitHold)
	}
	r.PreparedUntil = &until
	m.event("PREPARED %s (%s, %.1f GB) paused ahead of %s's likely %s request. Nothing is lost; resume with: %s. Resumed at %s if the request does not come.",
		r.ID, r.Name, r.MemMB/1024, short(p.session), p.kind, discover.HowToResume(r), until.Format("15:04"))
	m.notify("Paused "+r.Name+" ahead of a likely "+p.kind, "Nothing is lost. Resumed at "+until.Format("15:04")+" if it does not come.")
}

// collecting says whether a decision is about collecting one resource (pause or stop), as
// opposed to a plan (reclaim) or a pause made ahead of a prediction (prepare). Only those count
// as evidence for the auto-stop levels and the weekly receipt.
func collecting(d *model.Decision) bool { return d.Action != "reclaim" && d.Action != "prepare" }
