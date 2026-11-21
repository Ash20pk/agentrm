package sys

import "syscall"

// MemStats is a snapshot of the machine's memory (MB), disk, load and pressure.
type MemStats struct {
	TotalMB      float64 `json:"total_mb"`
	AvailableMB  float64 `json:"available_mb"` // usable without swapping: see availableMB (macOS), MemAvailable (Linux)
	CompressedMB float64 `json:"compressed_mb"`
	WiredMB      float64 `json:"wired_mb"`
	SwapTotalMB  float64 `json:"swap_total_mb"`
	SwapUsedMB   float64 `json:"swap_used_mb"`
	FreePercent  int     `json:"free_percent"` // available as a share of total
	DiskFreeGB   float64 `json:"disk_free_gb"` // on /
	Load1        float64 `json:"load1"`
	Cores        int     `json:"cores"`
	// PressureLevel is the kernel's own verdict: 1 normal, 2 warning, 4 critical.
	PressureLevel int     `json:"pressure_level"`
	Swapouts      float64 `json:"-"` // cumulative, in MB
	// SwapUnread: vm_stat could not be read, so Swapouts is not a reading.
	SwapUnread bool `json:"-"`
}

func DiskFreeGB(path string) float64 {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil {
		return -1
	}
	return float64(st.Bavail) * float64(st.Bsize) / (1 << 30)
}
