package main

import (
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// MemStats is a snapshot of the machine's memory, in MB.
type MemStats struct {
	TotalMB      float64 `json:"total_mb"`
	AvailableMB  float64 `json:"available_mb"` // free + inactive + purgeable + speculative
	CompressedMB float64 `json:"compressed_mb"`
	WiredMB      float64 `json:"wired_mb"`
	SwapTotalMB  float64 `json:"swap_total_mb"`
	SwapUsedMB   float64 `json:"swap_used_mb"`
	FreePercent  int     `json:"free_percent"` // from memory_pressure
}

var vmStatLine = regexp.MustCompile(`^(.+?):\s+(\d+)\.?$`)

func readMem() MemStats {
	var m MemStats
	if out, err := exec.Command("sysctl", "-n", "hw.memsize").Output(); err == nil {
		b, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
		m.TotalMB = b / 1048576
	}
	if out, err := exec.Command("vm_stat").Output(); err == nil {
		pageSize := 16384.0
		lines := strings.Split(string(out), "\n")
		if len(lines) > 0 {
			if mm := regexp.MustCompile(`page size of (\d+)`).FindStringSubmatch(lines[0]); mm != nil {
				pageSize, _ = strconv.ParseFloat(mm[1], 64)
			}
		}
		pages := map[string]float64{}
		for _, l := range lines[1:] {
			if mm := vmStatLine.FindStringSubmatch(strings.TrimSpace(l)); mm != nil {
				v, _ := strconv.ParseFloat(mm[2], 64)
				pages[mm[1]] = v
			}
		}
		mb := func(k string) float64 { return pages[k] * pageSize / 1048576 }
		m.AvailableMB = mb("Pages free") + mb("Pages inactive") + mb("Pages purgeable") + mb("Pages speculative")
		m.CompressedMB = mb("Pages occupied by compressor")
		m.WiredMB = mb("Pages wired down")
	}
	if out, err := exec.Command("sysctl", "-n", "vm.swapusage").Output(); err == nil {
		re := regexp.MustCompile(`total = ([\d.]+)M\s+used = ([\d.]+)M`)
		if mm := re.FindStringSubmatch(string(out)); mm != nil {
			m.SwapTotalMB, _ = strconv.ParseFloat(mm[1], 64)
			m.SwapUsedMB, _ = strconv.ParseFloat(mm[2], 64)
		}
	}
	m.FreePercent = -1
	if out, err := exec.Command("memory_pressure").Output(); err == nil {
		re := regexp.MustCompile(`free percentage: (\d+)%`)
		if mm := re.FindStringSubmatch(string(out)); mm != nil {
			m.FreePercent, _ = strconv.Atoi(mm[1])
		}
	}
	return m
}
