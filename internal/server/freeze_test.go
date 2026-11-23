package server

import (
	"testing"
	"time"
)

// A daemon that was frozen (kill -STOP) treats the connections it accepts on resuming as
// suspect, whether the late beat has run yet or not; one that kept running does not.
func TestFreezeWatch(t *testing.T) {
	now := time.Now()
	fw := &freezeWatch{last: now}
	if fw.justThawed() {
		t.Fatal("a running daemon counted as just thawed")
	}
	fw.last = now.Add(-5 * time.Second) // frozen: the beat has not run since
	if !fw.justThawed() {
		t.Fatal("accepting before the late beat was not suspect")
	}
	fw.beat(time.Now()) // the late beat
	if !fw.justThawed() {
		t.Fatal("just after the late beat was not suspect")
	}
	fw.thawedAt = time.Now().Add(-3 * time.Second)
	fw.last = time.Now()
	if fw.justThawed() {
		t.Fatal("still suspect long after resuming")
	}
}
