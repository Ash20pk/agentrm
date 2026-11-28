package manager

// Liveness by task. A live agent keeps what it started alive, but an agent outlives its tasks:
// the dev server started for one branch is still running, unused, while the agent works on the
// next. So a lease also belongs to a task (the repository and branch its session was on), and
// when the task looks finished — a pull request opened from it, or the session switched away
// from that branch — its holder is asked at once, and unless the lease is used again within
// owner_gone_grace_minutes it is released and collected like what an ended session left.
// Any use (a command naming it, traffic, a claim) cancels that: the task signal is a strong
// hint, never a reason to take something in use.

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"agentrm/internal/model"
)

var (
	prCreate     = regexp.MustCompile(`\b(gh\s+pr\s+create|glab\s+mr\s+create|hub\s+pull-request)\b`)
	branchSwitch = regexp.MustCompile(`\bgit\s+(?:checkout|switch)\s+(?:-[bBcC]\s+|--create\s+)?([^\s;&|-][^\s;&|]*)`)
)

// stampTask records which task a lease belongs to: where its session is working now.
func (m *Manager) stampTask(l *model.Lease) {
	if s := m.st.Sessions[l.Session]; s != nil && l.Branch == "" {
		l.Repo, l.Branch = s.Repo, s.Branch
	}
}

// noticeTaskEnd reads a command for signs that the session's current task is finished. Must
// hold mu.
func (m *Manager) noticeTaskEnd(session, cmd string, now time.Time) {
	s := m.st.Sessions[session]
	if s == nil || s.Branch == "" {
		return
	}
	switch {
	case prCreate.MatchString(cmd):
		m.taskDone(session, s.Repo, s.Branch, "a pull request was opened from it", now)
	case branchSwitch.MatchString(cmd):
		// Only a hint: `git checkout src/x.go` restores a file, and the words can sit inside a
		// commit message. The task ends when the branch is seen to have changed (branchMoved).
		if m.switchHint == nil {
			m.switchHint = map[string]time.Time{}
		}
		m.switchHint[session] = now
	}
}

// switchWindow is how long after a checkout or switch command a change of branch counts as
// the session leaving its task.
const switchWindow = 2 * time.Minute

// branchMoved is called when a session's requests start coming from another branch of the
// repository it was in, before the session is moved to it. The task on the branch it left is
// finished if the session itself just ran a checkout or switch: a session that only walked into
// another worktree of the repository left nothing. Must hold mu.
func (m *Manager) branchMoved(session string, s *model.Session, repo, branch string, now time.Time) {
	if s.Branch == "" || s.Repo != repo || s.Branch == branch {
		return
	}
	hint, ok := m.switchHint[session]
	delete(m.switchHint, session)
	if ok && now.Sub(hint) <= switchWindow {
		m.taskDone(session, s.Repo, s.Branch, "the session switched to "+branch, now)
	}
}

// taskDone marks every lease of the session's task on repo/branch as finished, and asks.
func (m *Manager) taskDone(session, repo, branch, why string, now time.Time) {
	grace := minutes(max(1, m.cfg.OwnerGoneGraceMinutes))
	for _, l := range m.st.Leases {
		if l.Session != session || l.Branch != branch || l.Repo != repo || l.TaskDoneAt != nil || len(l.Resources) == 0 {
			continue
		}
		t := now
		l.TaskDoneAt, l.TaskDoneWhy = &t, why
		m.askHolder(l, now, now.Add(grace), fmt.Sprintf("Your work on branch %s looks finished (%s), and", branch, why))
		m.event("TASKDONE lease %s (%s) of %s: branch %s, %s; released at %s unless used",
			l.ID, strings.Join(l.Resources, ", "), short(session), branch, why, now.Add(grace).Format("15:04"))
	}
}

// checkTaskDone releases leases whose task finished and that nobody used since. Called from
// checkLeases with the lease's latest use. It reports whether the lease was dropped.
func (m *Manager) checkTaskDone(l *model.Lease, used, now time.Time) bool {
	if l.TaskDoneAt == nil {
		return false
	}
	if used.After(*l.TaskDoneAt) {
		// Used after the task ended: it is still needed (by a follow-up, a review fix).
		m.event("TASKDONE cancelled for lease %s: it was used again", l.ID)
		l.TaskDoneAt, l.TaskDoneWhy = nil, ""
		return false
	}
	if now.Sub(*l.TaskDoneAt) < minutes(max(1, m.cfg.OwnerGoneGraceMinutes)) {
		return false
	}
	gone := *l.TaskDoneAt
	for _, rid := range l.Resources {
		if r := m.st.Resources[rid]; r != nil && r.Owner == "" {
			r.Owner, r.OwnerGoneAt = l.Session, &gone
		}
	}
	m.dropLease(l, "its task finished ("+l.TaskDoneWhy+") and it was not used since")
	return true
}
