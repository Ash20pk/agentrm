package manager

// Privileged actions: pinning, settings, priorities, and overriding another session's lease
// or resource. Only a person may take them. From the AgentRM app they run at once; from a
// terminal they wait for a click in the app (require_app_approval), because an agent with a
// shell can drive a terminal app and so act as you there.

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/discover"
	"agentrm/internal/model"
)

const approvalTTL = 2 * time.Minute

// privileged checks that c may do what. It returns errApproval when the app must confirm.
func (m *Manager) privileged(c api.Caller, what string) error {
	if !c.Human {
		return fmt.Errorf("only a person can %s; an agent cannot", what)
	}
	if m.cfg.RequireAppApproval && !c.ViaApp {
		return &errApproval{what: what}
	}
	if !c.ViaApp {
		m.event("OVERRIDE from %s: %s", orDash(c.App), what)
	}
	return nil
}

func (m *Manager) newApproval(path string, req api.Req, c api.Caller, what string) *model.Approval {
	var asMap map[string]any
	b, _ := json.Marshal(req)
	_ = json.Unmarshal(b, &asMap)
	now := m.now()
	a := &model.Approval{ID: newID(), What: what, Created: now, Expires: now.Add(approvalTTL),
		Request: asMap, Path: path, From: c.App, State: "pending"}
	m.st.Approvals[a.ID] = a
	m.dirty = true
	m.event("APPROVAL %s requested from %s: %s. Approve or deny it in AgentRM.", a.ID, orDash(c.App), what)
	m.notify("Approve: "+what+"?", "Asked from "+orDash(c.App)+". If that was not you, deny it.")
	return a
}

// approve runs or refuses a waiting request. Only the app may answer.
func (m *Manager) approve(c api.Caller, r api.Req) (any, []Effect, error) {
	if !c.ViaApp {
		return nil, nil, fmt.Errorf("approvals can only be answered in the AgentRM app")
	}
	a := m.st.Approvals[r.ApprovalID]
	if a == nil {
		return nil, nil, fmt.Errorf("no approval %s", r.ApprovalID)
	}
	if a.State != "pending" {
		return &api.ApprovalResp{Approval: a}, nil, nil
	}
	if m.now().After(a.Expires) {
		a.State = "expired"
		return &api.ApprovalResp{Approval: a}, nil, nil
	}
	if r.Approve == nil || !*r.Approve {
		a.State = "denied"
		m.event("DENIED %s: %s", a.ID, a.What)
		return &api.ApprovalResp{Approval: a}, nil, nil
	}
	if a.Vault != nil {
		return nil, nil, fmt.Errorf("a vault request is approved with Touch ID in the AgentRM app (it needs the account's secret)")
	}
	var req api.Req
	b, _ := json.Marshal(a.Request)
	_ = json.Unmarshal(b, &req)
	h := m.routes()[a.Path]
	you := api.Caller{Agent: "you", Session: "you", Human: true, ViaApp: true, App: "AgentRM.app"}
	resp, effects, err := h(you, req)
	a.State = "approved"
	out := &api.ApprovalResp{Approval: a, Result: resp}
	if err != nil {
		a.State, out.Error = "failed", err.Error()
	}
	m.event("APPROVED %s: %s", a.ID, a.What)
	return out, effects, nil
}

// approvalStatus lets the terminal that asked wait for the answer.
func (m *Manager) approvalStatus(c api.Caller, r api.Req) (any, []Effect, error) {
	a := m.st.Approvals[r.ApprovalID]
	if a == nil {
		return nil, nil, fmt.Errorf("no approval %s", r.ApprovalID)
	}
	if a.State == "pending" && m.now().After(a.Expires) {
		a.State = "expired"
	}
	return &api.ApprovalResp{Approval: a, Error: a.Error}, nil, nil
}

func (m *Manager) pin(c api.Caller, r api.Req) (any, []Effect, error) {
	on := r.Pinned == nil || *r.Pinned
	verb := "pin"
	if !on {
		verb = "unpin"
	}
	if err := m.privileged(c, verb+" "+r.ResourceID); err != nil {
		return nil, nil, err
	}
	if on {
		m.st.Pinned[r.ResourceID] = true
	} else {
		delete(m.st.Pinned, r.ResourceID)
	}
	var effects []Effect
	if res := m.st.Resources[r.ResourceID]; res != nil {
		if on && res.PausedAt != nil {
			effects = append(effects, m.thawEffect(res, "pinned"))
		}
		res.Pinned, res.OrphanSince = on, nil
	}
	m.event("PIN %s = %v", r.ResourceID, on)
	return &api.CountResp{N: 1}, effects, nil
}

func (m *Manager) setPriority(c api.Caller, r api.Req) (any, []Effect, error) {
	if r.Priority == nil {
		return nil, nil, fmt.Errorf("priority is required (0 normal, 1 urgent)")
	}
	target := r.Session
	if r.ResourceID != "" { // the CLI passes the target session here
		target = r.ResourceID
	}
	s := m.st.Sessions[target]
	if s == nil {
		return nil, nil, fmt.Errorf("no session %q (see agentrm status)", target)
	}
	p := min(max(*r.Priority, 0), 1)
	if err := m.privileged(c, fmt.Sprintf("set session %s to %s", target, map[int]string{0: "normal", 1: "urgent"}[p])); err != nil {
		return nil, nil, err
	}
	s.Priority = p
	m.event("PRIORITY %s = %d", target, p)
	return &api.CountResp{N: 1}, nil, nil
}

// setConfig changes settings live. The file is written by the persister, outside the lock.
func (m *Manager) setConfig(c api.Caller, r api.Req) (any, []Effect, error) {
	var changes []string
	cfg := m.cfg
	cfg.AutoStop = map[string]bool{}
	for k, v := range m.cfg.AutoStop {
		cfg.AutoStop[k] = v
	}
	if r.DryRun != nil {
		cfg.DryRun = *r.DryRun
		changes = append(changes, fmt.Sprintf("dry_run=%v", cfg.DryRun))
	}
	if r.GraceMinutes >= 1 {
		cfg.GraceMinutes = r.GraceMinutes
		changes = append(changes, fmt.Sprintf("grace=%dm", r.GraceMinutes))
	}
	if r.HeadroomGB > 0 {
		cfg.HeadroomGB = r.HeadroomGB
		changes = append(changes, fmt.Sprintf("headroom=%.1fGB", r.HeadroomGB))
	}
	if r.PauseMinutes != nil && *r.PauseMinutes >= 0 {
		cfg.PauseMinutes = *r.PauseMinutes
		changes = append(changes, fmt.Sprintf("pause=%dm", cfg.PauseMinutes))
	}
	if r.IdleMinutes >= 1 {
		cfg.IdleMinutes = r.IdleMinutes
		changes = append(changes, fmt.Sprintf("idle=%dm", r.IdleMinutes))
	}
	if len(r.AutoStop) > 0 {
		ch, err := m.applyAutoStop(&cfg, r.AutoStop)
		if err != nil {
			return nil, nil, err
		}
		changes = append(changes, ch...)
	}
	if r.RecordTrace != nil {
		cfg.RecordTrace = *r.RecordTrace
		changes = append(changes, fmt.Sprintf("record_trace=%v", cfg.RecordTrace))
	}
	if r.NeverStop != nil {
		cfg.NeverStop = nil
		for _, x := range r.NeverStop {
			if x = strings.TrimSpace(x); x != "" {
				cfg.NeverStop = append(cfg.NeverStop, x)
			}
		}
		changes = append(changes, fmt.Sprintf("never_stop=%v", cfg.NeverStop))
	}
	if len(changes) == 0 {
		return cfg, nil, nil
	}
	if err := m.privileged(c, "change settings: "+strings.Join(changes, ", ")); err != nil {
		return nil, nil, err
	}
	m.cfg = cfg
	m.cfgDirty = true
	m.applyLogLevel()
	m.applyRecording()
	m.event("CONFIG %s", strings.Join(changes, " "))
	return m.cfg, nil, nil
}

// stopNow stops a resource at once. An agent may stop what it holds or what nobody holds;
// stopping someone else's, or something pinned, is privileged.
func (m *Manager) stopNow(c api.Caller, r api.Req) (any, []Effect, error) {
	res := m.st.Resources[r.ResourceID]
	if res == nil {
		return nil, nil, fmt.Errorf("no running resource %q", r.ResourceID)
	}
	if m.pending[res.ID] != "" {
		return nil, nil, fmt.Errorf("%s is busy (%s in progress); try again in a moment", res.ID, m.pending[res.ID])
	}
	l := m.st.Leases[res.LeaseID]
	if res.Pinned || (l != nil && l.Session != c.Session) {
		holder := "pinned"
		if l != nil {
			holder = "held by " + l.Session
		}
		if !c.Human {
			return nil, nil, fmt.Errorf("%s is %s; an agent can stop only what it holds or what nobody holds", res.ID, holder)
		}
		if err := m.privileged(c, "stop "+res.ID+" ("+holder+")"); err != nil {
			return nil, nil, err
		}
	}
	if c.Session == "detached" {
		return nil, nil, fmt.Errorf("a detached process cannot stop anything")
	}
	if discover.SelfRestarting(res) {
		if !c.Human {
			return nil, nil, fmt.Errorf("%s is restarted only by you; an agent cannot", res.ID)
		}
		if busy := m.restartBlocker(res, m.now()); busy != "" {
			return nil, nil, fmt.Errorf("not restarting %s: %s", res.ID, busy)
		}
		if err := m.privileged(c, "restart "+res.Name+" (gives back ~"+gb(res.MemMB)+")"); err != nil {
			return nil, nil, err
		}
		m.pending[res.ID] = "stop"
		why := "restarted by " + c.Session
		m.decideAs("stop", res, why+" (by hand)", true)
		return &api.StopResp{Restart: res.Restart}, []Effect{{Op: "stop", Res: *res, Why: why, By: c.Session}}, nil
	}
	if !c.Human && l == nil && res.StartedBy != c.Session {
		// Nobody holds it, and this agent did not start it. An agent may stop only what AgentRM
		// itself would stop: not something you started, not an app, a VM or another agent.
		switch {
		case res.StartedBy == "you":
			return nil, nil, fmt.Errorf("%s was started by you; an agent cannot stop it", res.ID)
		case res.Kind == model.RKStaleAgent || res.AskOnly || !discover.CanAutoStop(res) || m.neverStop(res):
			why := discover.NoAutoStopWhy(res)
			if why == "" {
				why = "it is exempt in your settings"
			}
			return nil, nil, fmt.Errorf("%s is never stopped automatically (%s); an agent cannot stop it either", res.ID, why)
		}
	}
	m.pending[res.ID] = "stop"
	why := "stopped by " + c.Session
	m.decide("stop", res, why+" (by hand)")
	return &api.StopResp{Restart: "agentrm restore " + res.ID},
		[]Effect{{Op: "stop", Res: *res, Why: why, By: c.Session}}, nil
}

// restore brings back something AgentRM stopped. It is a request for memory like any other,
// so it goes through admission, and the restored resource is held by whoever asked.
func (m *Manager) restore(c api.Caller, r api.Req) (any, []Effect, error) {
	if c.Session == "detached" {
		return nil, nil, fmt.Errorf("a detached process cannot restore anything")
	}
	var st *model.Stopped
	for i := len(m.st.Stopped) - 1; i >= 0; i-- {
		s := m.st.Stopped[i]
		if r.All || s.Resource.ID == r.ResourceID {
			st = s
			break
		}
	}
	if st == nil {
		return nil, nil, fmt.Errorf("nothing stopped by AgentRM matches %q (see agentrm status --stopped)", r.ResourceID)
	}
	res := st.Resource
	if m.st.Resources[res.ID] != nil {
		return nil, nil, fmt.Errorf("%s is already running", res.ID)
	}
	kind := model.ResourceToLease[res.Kind]
	now := m.now()
	if ok, why, pos := m.admit(model.Waiter{Session: c.Session, Agent: c.Agent, Kind: kind, NeedMB: res.MemMB, Via: r.Via, Retry: true}, now); !ok {
		return &api.AcquireResp{Reason: why, Queue: pos}, nil, nil
	}
	purpose := r.Purpose
	if purpose == "" {
		purpose = "restored " + res.ID
	}
	l := &model.Lease{ID: newID(), Session: c.Session, Agent: c.Agent, Kind: kind, MemGB: res.MemMB / 1024,
		Purpose: purpose, Cwd: res.Cwd, TTL: m.cfg.DefaultTTLMinutes, Created: now,
		Expires: now.Add(minutes(m.cfg.DefaultTTLMinutes)), Verified: true, Evidence: "restored at its holder's request", LastUsed: now}
	m.st.Leases[l.ID] = l
	m.dockerOrigin[res.ID] = origin{session: c.Session, at: now} // bind it to this lease when it reappears
	out := make([]*model.Stopped, 0, len(m.st.Stopped))
	for _, s := range m.st.Stopped {
		if s != st {
			out = append(out, s)
		}
	}
	m.st.Stopped = out
	e := Effect{Op: "restore", Res: res, Why: "restore", By: c.Session, Stopped: st}
	e.Res.LeaseID = l.ID // released again if the restore fails
	return &api.AcquireResp{Granted: true, Lease: l}, []Effect{e}, nil
}
