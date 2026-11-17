package manager

// Backfilling. A large request at the head of the queue blocks everything behind it, even a
// two-minute build that would be long finished before the large one could start. The old fix
// from batch schedulers applies: a later request may run first if it fits in memory that is free
// now and is expected to finish soon. Here that means a build kind whose learned lifetime, nine
// times in ten, is within backfill_max_minutes; and each session may do it once per request at
// the head, so the head is never held back twice by the same session.

import (
	"time"

	"agentrm/internal/model"
)

// headWaiter is the request at the head of the queue that is not session's.
func headWaiter(session string, waiters []*model.Waiter) *model.Waiter {
	for _, w := range waiters {
		if w.Session != session {
			return w
		}
	}
	return nil
}

// canBackfill says whether a request that fits in free memory, but not around the memory
// earlier requests are waiting for, may go ahead of them.
func (m *Manager) canBackfill(session, kind, cwd string, me *model.Waiter, waiters []*model.Waiter) bool {
	if !m.cfg.Backfill || !model.BuildKinds[kind] {
		return false
	}
	head := headWaiter(session, waiters)
	if head == nil || (me != nil && ahead(me, head)) {
		return false
	}
	if m.backfilled[head][session] {
		return false
	}
	p90, ok := m.lifetime(kind, cwd, 0.9)
	return ok && p90 <= minutes(max(1, m.cfg.BackfillMaxMinutes))
}

// noteBackfill records that session went ahead of the current head of the queue.
func (m *Manager) noteBackfill(session string, waiters []*model.Waiter, now time.Time) {
	head := headWaiter(session, waiters)
	if head == nil {
		return
	}
	for w := range m.backfilled {
		live := false
		for _, x := range m.waiters {
			live = live || x == w
		}
		if !live {
			delete(m.backfilled, w)
		}
	}
	if m.backfilled[head] == nil {
		m.backfilled[head] = map[string]bool{}
	}
	m.backfilled[head][session] = true
	m.event("BACKFILL %s goes ahead of %s's %.1f GB %s request: a short build that fits in free memory",
		short(session), short(head.Session), head.NeedMB/1024, head.Kind)
}
