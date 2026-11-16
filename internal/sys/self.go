package sys

import (
	"runtime"
	"sync"
	"syscall"
	"time"
)

// SelfUsage is what the daemon itself costs the machine.
type SelfUsage struct {
	RSSMB      float64 `json:"rss_mb"`      // peak resident memory
	HeapMB     float64 `json:"heap_mb"`     // live Go heap
	CPUPercent float64 `json:"cpu_percent"` // average over the last minute or so
	Goroutines int     `json:"goroutines"`
	SweepMS    float64 `json:"sweep_ms"` // duration of the last sweep
}

var selfState struct {
	sync.Mutex
	lastCPU time.Duration
	lastAt  time.Time
	pct     float64
}

// Self measures the daemon's own cost. Call it periodically; the CPU figure is the average
// since the previous call.
func Self() SelfUsage {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	cpu := time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
	now := time.Now()
	selfState.Lock()
	if !selfState.lastAt.IsZero() {
		if dt := now.Sub(selfState.lastAt); dt > time.Second {
			selfState.pct = 100 * float64(cpu-selfState.lastCPU) / float64(dt)
		}
	}
	selfState.lastCPU, selfState.lastAt = cpu, now
	pct := selfState.pct
	selfState.Unlock()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return SelfUsage{
		RSSMB:      float64(ru.Maxrss) / 1048576, // bytes on macOS
		HeapMB:     float64(ms.HeapAlloc) / 1048576,
		CPUPercent: pct,
		Goroutines: runtime.NumGoroutine(),
	}
}
