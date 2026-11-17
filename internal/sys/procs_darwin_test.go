package sys

import "testing"

func TestParseLstartAndStableStarts(t *testing.T) {
	got, ok := ParseLstart("Tue Nov 17 00:30:24 2026")
	if !ok || got.Day() != 17 || got.Hour() != 0 || got.Second() != 24 {
		t.Fatalf("ParseLstart: %v %v", got, ok)
	}
	if _, ok := ParseLstart("Sat Oct  3 09:01:02 2026"); !ok {
		t.Fatal("single-digit day not parsed")
	}
	a, b := ListProcs(), ListProcs()
	if a == nil || b == nil {
		t.Fatal("no table")
	}
	moved := 0
	for pid, p := range a {
		if q := b[pid]; q != nil && !p.Start.Equal(q.Start) {
			moved++
		}
	}
	if moved > 0 {
		t.Fatalf("%d processes changed start time between two samples", moved)
	}
}
