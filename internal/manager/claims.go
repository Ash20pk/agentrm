package manager

import (
	"fmt"
	"sort"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/evidence"
	"agentrm/internal/model"
)

func (m *Manager) claim(c api.Caller, r api.Req) (any, []Effect, error) {
	if c.Session == "detached" {
		return nil, nil, fmt.Errorf("a detached process cannot claim anything")
	}
	res := m.st.Resources[r.ResourceID]
	if res == nil {
		return nil, nil, fmt.Errorf("no running resource %q (see agentrm status)", r.ResourceID)
	}
	if op := m.pending[res.ID]; op == "stop" {
		return nil, nil, fmt.Errorf("%s is being stopped right now; it can be restored with agentrm restore %s", res.ID, res.ID)
	}
	now := m.now()
	var effects []Effect
	if res.PausedAt != nil {
		effects = append(effects, m.thawEffect(res, "claimed by "+c.Session))
	}
	if res.LeaseID != "" {
		if l := m.st.Leases[res.LeaseID]; l != nil {
			if l.Session != c.Session {
				return nil, nil, fmt.Errorf("%s is already held by lease %s (%s: %s)", res.ID, l.ID, l.Session, l.Purpose)
			}
			// The holder confirming it still needs it, perhaps with evidence it lacked before.
			if !l.Verified {
				if ok, ev := m.claimEvidence(res, c); ok {
					l.Verified, l.Evidence = true, ev
					l.TTL = max(l.TTL, m.cfg.DefaultTTLMinutes)
				}
			}
			if r.UntilMinutes > 0 {
				u := now.Add(minutes(r.UntilMinutes))
				l.Until = &u
			} else if l.Until != nil && now.After(*l.Until) {
				l.Until = nil // confirming after the declared end extends it open-ended
			}
			l.LastUsed, l.IdleAsked = now, nil
			l.Expires = now.Add(minutes(l.TTL))
			delete(m.st.Questions, "idle:"+l.ID)
			m.event("CONFIRM %s by %s (lease %s, verified=%v)", res.ID, c.Session, l.ID, l.Verified)
			return &api.ClaimResp{Lease: l, Verified: l.Verified, Evidence: l.Evidence}, effects, nil
		}
	}
	if r.Purpose == "" {
		return nil, nil, fmt.Errorf("say why you need it: --purpose")
	}
	ttl := r.TTLMinutes
	if ttl == 0 {
		ttl = m.cfg.DefaultTTLMinutes
	}
	verified, ev := m.claimEvidence(res, c)
	if !verified {
		ttl = min(ttl, m.cfg.UnverifiedTTLMinutes)
	}
	cwd := r.Cwd
	if cwd == "" {
		cwd = res.Cwd
	}
	l := &model.Lease{ID: newID(), Session: c.Session, Agent: c.Agent, Kind: "claim:" + res.Kind,
		MemGB: res.MemMB / 1024, Purpose: r.Purpose, Cwd: cwd, TTL: ttl, Created: now,
		Expires: now.Add(minutes(ttl)), Resources: []string{res.ID}, Verified: verified, Evidence: ev, LastUsed: now}
	if r.UntilMinutes > 0 {
		u := now.Add(minutes(r.UntilMinutes))
		l.Until = &u
	}
	m.st.Leases[l.ID] = l
	res.LeaseID, res.OrphanSince, res.Urgent = l.ID, nil, false
	delete(m.st.Questions, res.ID)
	tag := "verified"
	if !verified {
		tag = "UNVERIFIED"
	}
	m.event("CLAIM %s by %s as lease %s (%s: %s): %s", res.ID, c.Session, l.ID, tag, ev, r.Purpose)
	return &api.ClaimResp{Lease: l, Verified: verified, Evidence: ev}, effects, nil
}

// claimEvidence decides whether a claim is backed by what the machine has seen.
func (m *Manager) claimEvidence(r *model.Resource, c api.Caller) (bool, string) {
	switch {
	case c.Human:
		return true, "claimed by a person, not an agent"
	case r.StartedBy != "" && r.StartedBy == c.Session:
		return true, "this session started it"
	}
	if t, ok := m.st.Touches[r.ID][c.Session]; ok && m.now().Sub(t) < minutes(m.cfg.ClaimWindowMinutes) {
		return true, "this session used it at " + t.Format("15:04")
	}
	return false, fmt.Sprintf("no sign this session has used it in the last %d min; "+
		"held for %d min unless a command of this session uses it", m.cfg.ClaimWindowMinutes, m.cfg.UnverifiedTTLMinutes)
}

func (m *Manager) disown(c api.Caller, r api.Req) (any, []Effect, error) {
	if q := m.st.Questions[r.ResourceID]; q != nil {
		q.Answers[c.Session] = "not mine"
	}
	m.event("DISOWN %s by %s", r.ResourceID, c.Session)
	return &api.CountResp{N: 1}, nil, nil
}

// inbox returns the questions this session has not seen yet (all of them with All).
func (m *Manager) inbox(c api.Caller, r api.Req) (any, []Effect, error) {
	var out []string
	for _, q := range m.st.Questions {
		if q.Answers[c.Session] != "" || (!r.All && q.Delivered[c.Session]) || (q.For != "" && q.For != c.Session) {
			continue
		}
		q.Delivered[c.Session] = true
		out = append(out, q.Text)
	}
	sort.Strings(out)
	return &api.InboxResp{Questions: out}, nil, nil
}

// touchCmd records a command an agent is about to run and which resources it uses. A paused
// resource it uses is resumed before the command runs.
func (m *Manager) touchCmd(c api.Caller, r api.Req) (any, []Effect, error) {
	now := m.now()
	if c.Session == "detached" || r.Cmd == "" {
		return &api.TouchResp{}, nil, nil
	}
	rec := append(m.recent[c.Session], cmdRecord{Time: now, Cmd: trunc(r.Cmd, 500), Cwd: r.Cwd})
	if len(rec) > 50 {
		rec = rec[len(rec)-50:]
	}
	m.recent[c.Session] = rec
	ids := evidence.TouchedBy(r.Cmd, r.Cwd, m.st.Resources, evidence.CurrentKubeContext())
	resp := &api.TouchResp{Touched: ids}
	var effects []Effect
	for _, id := range ids {
		if res := m.st.Resources[id]; res != nil && res.PausedAt != nil {
			effects = append(effects, m.thawEffect(res, "a command of "+short(c.Session)+" is about to use it"))
			resp.Thawed = append(resp.Thawed, id)
		}
		if m.st.Touches[id] == nil {
			m.st.Touches[id] = map[string]time.Time{}
		}
		m.st.Touches[id][c.Session] = now
	}
	if r.Gate {
		if kind := m.classifier.Classify(r.Cmd); kind != "" {
			resp.Kind = kind
			out, _, err := m.acquire(c, api.Req{Kind: kind, Cmd: r.Cmd, Cwd: r.Cwd, Via: r.Via,
				Purpose: "auto: " + trunc(oneLine(r.Cmd), 80)})
			if err != nil {
				return nil, effects, err
			}
			resp.Acquire = out.(*api.AcquireResp)
		}
	}
	return resp, effects, nil
}

func oneLine(s string) string {
	for i, c := range s {
		if c == '\n' {
			return s[:i] + " …"
		}
	}
	return s
}
