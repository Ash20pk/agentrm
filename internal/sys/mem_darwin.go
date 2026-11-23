package sys

import (
	"encoding/binary"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

var vmStatLine = regexp.MustCompile(`^(.+?):\s+(\d+)\.?$`)
var pageSizeRe = regexp.MustCompile(`page size of (\d+)`)

// ReadMem takes a full snapshot. vm_stat is the one external command left (see D3).
func ReadMem() MemStats {
	m := ReadFast()
	if out, err := Run(5*time.Second, vmStatPath); err == nil {
		pageSize := 16384.0
		lines := strings.Split(out, "\n")
		if mm := pageSizeRe.FindStringSubmatch(lines[0]); mm != nil {
			pageSize, _ = strconv.ParseFloat(mm[1], 64)
		}
		pages := map[string]float64{}
		for _, l := range lines[1:] {
			if mm := vmStatLine.FindStringSubmatch(strings.TrimSpace(l)); mm != nil {
				v, _ := strconv.ParseFloat(mm[2], 64)
				pages[mm[1]] = v
			}
		}
		mb := func(k string) float64 { return pages[k] * pageSize / 1048576 }
		m.AvailableMB = availableMB(mb)
		m.CompressedMB = mb("Pages occupied by compressor")
		m.Swapouts = mb("Swapouts")
		m.WiredMB = mb("Pages wired down")
	} else {
		m.SwapUnread = true
	}
	if m.TotalMB > 0 {
		m.FreePercent = int(100 * m.AvailableMB / m.TotalMB)
	}
	return m
}

// ReadFast reads what sysctl gives directly, with no external command: total memory, swap,
// pressure level, load, cores and disk. Cheap enough to call on every request.
func ReadFast() MemStats {
	var m MemStats
	if v, err := unix.SysctlUint64("hw.memsize"); err == nil {
		m.TotalMB = float64(v) / 1048576
	}
	// struct xsw_usage { u64 total, avail, used; u32 pagesize; bool encrypted; }
	if b, err := unix.SysctlRaw("vm.swapusage"); err == nil && len(b) >= 24 {
		m.SwapTotalMB = float64(binary.LittleEndian.Uint64(b[0:8])) / 1048576
		m.SwapUsedMB = float64(binary.LittleEndian.Uint64(b[16:24])) / 1048576
	}
	m.PressureLevel = PressureLevel()
	m.Load1, m.Cores = LoadAvg()
	m.DiskFreeGB = DiskFreeGB("/")
	return m
}

// PressureLevel is the kernel's memory-pressure level: 1 normal, 2 warning, 4 critical.
func PressureLevel() int {
	if v, err := unix.SysctlUint32("kern.memorystatus_vm_pressure_level"); err == nil && v > 0 {
		return int(v)
	}
	return 1
}

// LoadAvg returns the 1-minute load average and the number of cores.
func LoadAvg() (float64, int) {
	cores := 0
	if v, err := unix.SysctlUint32("hw.ncpu"); err == nil {
		cores = int(v)
	}
	// struct loadavg { fixpt_t ldavg[3] (u32); long fscale; } with padding to 8 bytes.
	if b, err := unix.SysctlRaw("vm.loadavg"); err == nil && len(b) >= 24 {
		ld := binary.LittleEndian.Uint32(b[0:4])
		fscale := binary.LittleEndian.Uint64(b[16:24])
		if fscale > 0 {
			return float64(ld) / float64(fscale), cores
		}
	}
	return 0, cores
}
