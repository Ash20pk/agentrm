package sys

import "testing"

// vm_stat on a 16 GB Mac, 2026-11-18 08:16, swap 78% full (16 KB pages).
func TestAvailableIsWhatCanBeHadWithoutSwapping(t *testing.T) {
	pages := map[string]float64{
		"Pages free": 27077, "Pages active": 260230, "Pages inactive": 253838, "Pages speculative": 5757,
		"Pages wired down": 218377, "Pages purgeable": 3447, "File-backed pages": 114223,
		"Anonymous pages": 405602, "Pages stored in compressor": 515279, "Pages occupied by compressor": 242809,
	}
	mb := func(k string) float64 { return pages[k] * 16384 / 1048576 }
	got := availableMB(mb)
	old := mb("Pages free") + mb("Pages inactive") + mb("Pages purgeable") + mb("Pages speculative")
	// free 423 + file cache 1785 + purgeable 54 + idle app memory 2181 compressed at 2.12:1 (1153)
	if got < 3300 || got > 3500 {
		t.Fatalf("available %.0f MB, want ~3415", got)
	}
	if got >= old {
		t.Fatalf("the new figure (%.0f) should be below free+inactive (%.0f), which counts app memory as free", got, old)
	}
	// With no compressor in use, idle app memory earns the default 2:1 credit; with no
	// file-backed split, the old estimate is used.
	if v := availableMB(func(k string) float64 {
		return map[string]float64{"Pages free": 100, "File-backed pages": 50, "Pages inactive": 250}[k]
	}); v != 100+50+100 {
		t.Fatalf("default ratio: %.0f", v)
	}
}
