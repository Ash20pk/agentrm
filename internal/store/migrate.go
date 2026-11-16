package store

import (
	"time"

	"agentrm/internal/model"
)

// minLearnMB is the smallest peak worth learning from; smaller learned values came from leases
// that never ran anything (a bug fixed in state version 2).
const minLearnMB = 100

// Migrate brings a loaded state to model.StateVersion and resets what does not survive a
// restart. It returns a note for each migration applied.
func Migrate(st *model.State, now time.Time) []string {
	var notes []string
	fresh := model.NewState()
	if st.Leases == nil {
		st.Leases = fresh.Leases
	}
	if st.Sessions == nil {
		st.Sessions = fresh.Sessions
	}
	if st.Resources == nil {
		st.Resources = fresh.Resources
	}
	if st.Pinned == nil {
		st.Pinned = fresh.Pinned
	}
	if st.Touches == nil {
		st.Touches = fresh.Touches
	}
	if st.Learned == nil {
		st.Learned = fresh.Learned
	}
	if st.LearnedDev == nil {
		st.LearnedDev = fresh.LearnedDev
	}
	if st.AgentSessions == nil {
		st.AgentSessions = fresh.AgentSessions
	}
	if st.Approvals == nil {
		st.Approvals = fresh.Approvals
	}

	// v0/v1 → v2: leases from before use tracking have no LastUsed; learned values below the
	// minimum came from leases that never ran anything.
	if st.Version < 2 {
		for _, l := range st.Leases {
			if l.LastUsed.IsZero() {
				l.LastUsed, l.IdleAsked = now, nil
				if l.Evidence == "" {
					l.Evidence = "held before evidence tracking"
				}
			}
		}
		for k, mb := range st.Learned {
			if mb < minLearnMB {
				delete(st.Learned, k)
				delete(st.LearnedDev, k)
			}
		}
		notes = append(notes, "migrated state to version 2 (use tracking, learned estimates cleaned)")
	}
	// v2 → v3: stopped list, approvals and decisions (initialised above); nothing to convert.
	if st.Version < 3 {
		notes = append(notes, "migrated state to version 3 (restore list, approvals, decision log)")
	}
	st.Version = model.StateVersion

	// Not carried across a restart: orphan clocks restart, and questions are asked again.
	for _, r := range st.Resources {
		r.OrphanSince = nil
	}
	st.Questions = fresh.Questions
	// Approvals expire; a restart is a good moment to drop the stale ones.
	for id, a := range st.Approvals {
		if now.After(a.Expires) {
			delete(st.Approvals, id)
		}
	}
	return notes
}
