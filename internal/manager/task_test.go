package manager

import (
	"strings"
	"testing"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/model"
)

// onBranch makes a session's next requests come from repo/branch, as the daemon would read it.
func onBranch(m *Manager, session, branch string) {
	m.mu.Lock()
	m.resolve(facts{pid: 900, agentPID: 500, agent: "claude"}, &api.Req{Session: session, Via: "hook", Repo: "/src/app", Branch: branch})
	m.mu.Unlock()
}

func TestFinishedTaskReleasesWhatItStarted(t *testing.T) {
	m, c := newTest(t, 8)
	m.cfg.DryRun = false
	got := fakeEffects(t, nil)
	onBranch(m, "s", "feat-a")
	la := acquire(t, m, agent("s"), "server", 0.5)
	ra := addRes(m, "srv:3000", model.RKServer, 600, la.Lease.ID)
	if l := m.st.Leases[la.Lease.ID]; l.Branch != "feat-a" || l.Repo != "/src/app" {
		t.Fatalf("lease not stamped with its task: %+v", l)
	}
	// The agent opens the PR, then starts the next task on another branch.
	m.touchCmd(agent("s"), api.Req{Cmd: "git push -u origin feat-a && gh pr create --fill"})
	onBranch(m, "s", "feat-b")
	lb := acquire(t, m, agent("s"), "compose", 0.5)
	rb := addRes(m, "compose:b", model.RKContainers, 600, lb.Lease.ID)

	out, _, _ := m.inbox(agent("s"), api.Req{})
	if q := out.(*api.InboxResp).Questions; len(q) != 1 || !strings.Contains(q[0], "feat-a looks finished") {
		t.Fatalf("the holder was not asked when its task finished: %v", q)
	}
	c.Add(minutes(m.cfg.OwnerGoneGraceMinutes) + time.Second)
	m.checkLeases(c.Now())
	if ra.LeaseID != "" || ra.Owner != "s" {
		t.Fatalf("the finished task's server kept its lease: %+v", ra)
	}
	if rb.LeaseID == "" {
		t.Fatal("the next task's stack lost its lease")
	}
	m.applyEffects(RunEffects(m.checkOrphans(c.Now()))) // unclaimed from now, on the owner-gone clock
	c.Add(minutes(m.cfg.OwnerGoneGraceMinutes) + time.Second)
	m.applyEffects(RunEffects(m.checkOrphans(c.Now())))
	paused := false
	for _, e := range *got {
		paused = paused || e.Res.ID == ra.ID && e.Op == "pause"
	}
	if !paused {
		t.Fatalf("the finished task's server was not collected on the fast path: %+v", *got)
	}
}

func TestUseAfterTheTaskEndsKeepsIt(t *testing.T) {
	m, c := newTest(t, 8)
	onBranch(m, "s", "feat-a")
	la := acquire(t, m, agent("s"), "server", 0.5)
	ra := addRes(m, "srv:3000", model.RKServer, 600, la.Lease.ID)
	ra.Ports = []string{"3000"}
	// Restoring a file, or the words in a commit message, leave no branch.
	m.touchCmd(agent("s"), api.Req{Cmd: "git checkout main -- go.sum"})
	m.touchCmd(agent("s"), api.Req{Cmd: `git commit -m "explain git checkout usage"`})
	onBranch(m, "s", "feat-a")
	if m.st.Leases[la.Lease.ID].TaskDoneAt != nil {
		t.Fatal("a checkout of a file ended the task")
	}
	m.touchCmd(agent("s"), api.Req{Cmd: "git switch main"})
	onBranch(m, "s", "main") // its next request comes from main
	if m.st.Leases[la.Lease.ID].TaskDoneAt == nil {
		t.Fatal("switching branch did not end the task")
	}
	// A review comment comes in; the agent checks the server again.
	c.Add(time.Minute)
	m.touchCmd(agent("s"), api.Req{Cmd: "curl -s localhost:3000/api"})
	c.Add(minutes(m.cfg.OwnerGoneGraceMinutes))
	m.checkLeases(c.Now())
	if ra.LeaseID == "" || m.st.Leases[la.Lease.ID].TaskDoneAt != nil {
		t.Fatal("a server used after its task ended was released")
	}
	// Another worktree of the repository, with no checkout run, ends nothing either.
	onBranch(m, "s", "feat-a")
	c.Add(switchWindow + time.Second)
	onBranch(m, "s", "other-worktree")
	if m.st.Leases[la.Lease.ID].TaskDoneAt != nil {
		t.Fatal("a change of branch the session did not make ended the task")
	}
}
