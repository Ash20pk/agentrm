package manager

// Invariants, checked after every step of random sequences of what agents and people do.
//
//  1. Nothing is stopped by AgentRM while it is held by a lease, pinned, never-stop, ask-only,
//     a VM, or used in the last grace period.
//  2. A grant never promises memory the machine does not have: every grant, on request or from
//     the queue, fits in the headroom at the moment it is made.
//  3. The queue drains: once memory is plentiful and sessions keep talking, every queued
//     request is granted or dropped, within a few sweeps.
//  4. Leases and resources agree: a resource's lease exists and lists it.
//  5. Nothing you started is paused or stopped by AgentRM.

import (
	"fmt"
	"math/rand"
	"strconv"
	"testing"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/discover"
	"agentrm/internal/model"
	"agentrm/internal/sys"
)

type world struct {
	t       *testing.T
	m       *Manager
	c       *clock
	rng     *rand.Rand
	agents  map[string]int // session -> agent pid (0: exited)
	running map[string]*model.Resource
	effects *[]Effect
	log     []string
}

var kinds = []string{"cluster", "compose", "server", "cargo-build", "docker-build", "model"}

func newWorld(t *testing.T, seed int64) *world {
	m, c := newTest(t, 8)
	m.cfg.DryRun = false
	m.cfg.MaxWaitMinutes = -1 // invariant 2 is about admission; the wait limit grants on purpose
	m.notify = func(string, string) {}
	w := &world{t: t, m: m, c: c, rng: rand.New(rand.NewSource(seed)), agents: map[string]int{},
		running: map[string]*model.Resource{}, effects: fakeEffects(t, nil)}
	m.onGrant = func(need, head float64) {
		if need > head+1 {
			t.Fatalf("invariant 2: granted %.0f MB with %.0f MB of headroom\n%v", need, head, w.tail())
		}
	}
	for i := 0; i < 4; i++ {
		w.agents["s"+strconv.Itoa(i)] = 500 + i
	}
	return w
}

func (w *world) session() string { return "s" + strconv.Itoa(w.rng.Intn(4)) }

func (w *world) procs() map[int]*sys.Proc {
	start := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	ps := map[int]*sys.Proc{
		1:   {PID: 1, Exe: "/sbin/launchd"},
		400: {PID: 400, PPID: 1, Exe: "/Applications/Warp.app/Contents/MacOS/stable", Path: "/Applications/Warp.app/Contents/MacOS/stable"},
	}
	for _, pid := range w.agents {
		if pid > 0 {
			ps[pid] = &sys.Proc{PID: pid, PPID: 400, Exe: "/usr/local/bin/claude", Args: "claude", Start: start}
		}
	}
	for _, r := range w.running {
		if r.PID > 0 {
			ps[r.PID] = &sys.Proc{PID: r.PID, PPID: 1, Exe: "node", Args: "node " + r.ID, Start: start, RSSMB: r.MemMB}
		}
	}
	return ps
}

func (w *world) sweep(est map[string]bool) {
	var found []*model.Resource
	for _, r := range w.running {
		c := *r
		found = append(found, &c)
	}
	w.m.sweepWith(w.procs(), discover.Result{Resources: found, Failed: map[string]error{}}, w.m.mem, est, nil)
}

func (w *world) acquire(session string) {
	kind := kinds[w.rng.Intn(len(kinds))]
	gb := 0.25 + w.rng.Float64()*3
	before := len(w.m.st.Leases)
	out, _, err := w.m.acquire(agent(session), api.Req{Kind: kind, MemGB: gb, Purpose: "prop", Via: "hook"})
	if err != nil {
		return
	}
	r := out.(*api.AcquireResp)
	w.log = append(w.log, fmt.Sprintf("acquire %s %s %.1f -> %v (leases %d)", session, kind, gb, r.Granted, before))
}

func (w *world) tail() []string {
	if len(w.log) > 15 {
		return w.log[len(w.log)-15:]
	}
	return w.log
}

// step does one random thing.
func (w *world) step(i int) {
	m := w.m
	switch op := w.rng.Intn(14); op {
	case 0, 1, 2:
		w.acquire(w.session())
	case 3:
		s := w.session()
		_, _, _ = m.release(agent(s), api.Req{})
		w.log = append(w.log, "release "+s)
	case 4: // something starts: a resource, bound to a lease or not
		id := fmt.Sprintf("srv:%d", 3000+w.rng.Intn(8))
		if w.running[id] == nil {
			w.running[id] = &model.Resource{ID: id, Kind: model.RKServer, Source: "procs", Name: "node", PID: 7000 + i,
				MemMB: 100 + float64(w.rng.Intn(900)), Ports: []string{id[4:]}}
			w.log = append(w.log, "start "+id)
		}
	case 5: // something exits by itself
		for id := range w.running {
			delete(w.running, id)
			w.log = append(w.log, "exit "+id)
			break
		}
	case 6: // an agent exits, or comes back
		s := w.session()
		if w.agents[s] > 0 {
			pid := w.agents[s]
			w.agents[s] = 0
			m.AgentExited(pid, w.procs())
			w.log = append(w.log, "agent exits "+s)
		} else {
			w.agents[s] = 500 + w.rng.Intn(4)
			m.touchSession(s, "claude", "")
			w.log = append(w.log, "agent back "+s)
		}
	case 7: // a session ends by hook, or /clear
		s := w.session()
		reason := ""
		if w.rng.Intn(3) == 0 {
			reason = "clear"
		}
		_, _, _ = m.end(agent(s), api.Req{Reason: reason})
		w.log = append(w.log, "end "+s+" "+reason)
	case 8: // a session claims something
		for id := range w.running {
			if r := m.st.Resources[id]; r != nil && r.LeaseID == "" {
				s := w.session()
				_, _, _ = m.claim(agent(s), api.Req{ResourceID: id, Purpose: "mine"})
				w.log = append(w.log, "claim "+id+" by "+s)
			}
			break
		}
	case 9: // you pin something
		for id := range w.running {
			if r := m.st.Resources[id]; r != nil && w.rng.Intn(3) == 0 {
				r.Pinned = true
				w.running[id].Pinned = true
				w.log = append(w.log, "pin "+id)
			}
			break
		}
	case 10: // memory moves
		m.mem.AvailableMB = max(512, m.mem.AvailableMB+float64(w.rng.Intn(4096)-2048))
		w.log = append(w.log, fmt.Sprintf("mem %.1f GB", m.mem.AvailableMB/1024))
	default: // time passes, with a sweep; sometimes something is used
		w.c.Add(time.Duration(30+w.rng.Intn(300)) * time.Second)
		var est map[string]bool
		if w.rng.Intn(4) == 0 {
			for _, r := range w.running {
				est = map[string]bool{r.Ports[0]: true}
				break
			}
		}
		w.sweep(est)
		w.log = append(w.log, "sweep")
	}
	w.checkInvariants()
}

func (w *world) checkInvariants() {
	m := w.m
	// 1 and 5: what was stopped or paused by AgentRM was fair game.
	for _, e := range *w.effects {
		if e.By != "agentrm" || (e.Op != "stop" && e.Op != "pause") {
			continue
		}
		r := e.Res
		switch {
		case r.LeaseID != "":
			w.t.Fatalf("invariant 1: %s %s while it held lease %s\n%v", e.Op, r.ID, r.LeaseID, w.tail())
		case r.Pinned || r.AskOnly || r.Kind == model.RKVM && e.Op == "stop":
			w.t.Fatalf("invariant 1: %s %s, which is pinned, ask-only or a VM\n%v", e.Op, r.ID, w.tail())
		case r.StartedBy == "you":
			w.t.Fatalf("invariant 5: %s %s, which you started\n%v", e.Op, r.ID, w.tail())
		}
	}
	*w.effects = nil
	// 4: leases and resources agree.
	for _, r := range m.st.Resources {
		if r.LeaseID == "" {
			continue
		}
		l := m.st.Leases[r.LeaseID]
		if l == nil {
			w.t.Fatalf("invariant 4: %s names lease %s, which does not exist\n%v", r.ID, r.LeaseID, w.tail())
		}
		found := false
		for _, id := range l.Resources {
			found = found || id == r.ID
		}
		if !found {
			w.t.Fatalf("invariant 4: lease %s does not list %s, which names it\n%v", l.ID, r.ID, w.tail())
		}
	}
}

// drain: plenty of memory, everyone talking, a few sweeps: the queue must empty.
func (w *world) drain() {
	m := w.m
	m.mem.AvailableMB, m.swapRate, m.mem.PressureLevel = 64*1024, 0, 1
	m.mem.TotalMB = 96 * 1024
	for i := 0; i < 5; i++ {
		for s := range w.agents {
			m.touchSession(s, "claude", "")
		}
		w.c.Add(30 * time.Second)
		w.sweep(nil)
	}
	if ws := m.liveWaiters(m.now()); len(ws) > 0 {
		w.t.Fatalf("invariant 3: %d request(s) still queued with 64 GB free: %+v\n%v", len(ws), *ws[0], w.tail())
	}
}

func TestInvariantsUnderRandomSequences(t *testing.T) {
	seeds := 300
	if testing.Short() {
		seeds = 40
	}
	for seed := int64(1); seed <= int64(seeds); seed++ {
		t.Run(strconv.FormatInt(seed, 10), func(t *testing.T) {
			w := newWorld(t, seed)
			for i := 0; i < 250; i++ {
				w.step(i)
			}
			w.drain()
		})
	}
}
