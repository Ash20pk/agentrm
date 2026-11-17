package sys

import (
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The sysctl structs are parsed by byte offset; check them against the sysctl command.
func TestFastReadsMatchSysctl(t *testing.T) {
	m := ReadFast()
	out, _ := exec.Command("sysctl", "-n", "vm.swapusage").Output()
	mm := regexp.MustCompile(`total = ([\d.]+)M\s+used = ([\d.]+)M`).FindStringSubmatch(string(out))
	total, _ := strconv.ParseFloat(mm[1], 64)
	used, _ := strconv.ParseFloat(mm[2], 64)
	if d := m.SwapTotalMB - total; d > 1 || d < -1 {
		t.Errorf("swap total %.1f, sysctl says %.1f", m.SwapTotalMB, total)
	}
	if d := m.SwapUsedMB - used; d > 64 || d < -64 { // it moves between the two reads
		t.Errorf("swap used %.1f, sysctl says %.1f", m.SwapUsedMB, used)
	}
	out, _ = exec.Command("sysctl", "-n", "vm.loadavg").Output()
	l1, _ := strconv.ParseFloat(strings.Fields(strings.Trim(string(out), "{} \n"))[0], 64)
	if d := m.Load1 - l1; d > 1 || d < -1 {
		t.Errorf("load %.2f, sysctl says %.2f", m.Load1, l1)
	}
	out, _ = exec.Command("sysctl", "-n", "hw.ncpu").Output()
	if n, _ := strconv.Atoi(strings.TrimSpace(string(out))); n != m.Cores {
		t.Errorf("cores %d, sysctl says %d", m.Cores, n)
	}
	out, _ = exec.Command("sysctl", "-n", "kern.memorystatus_vm_pressure_level").Output()
	if n, _ := strconv.Atoi(strings.TrimSpace(string(out))); n != m.PressureLevel {
		t.Errorf("pressure %d, sysctl says %d", m.PressureLevel, n)
	}
	t.Logf("total %.0f MB, swap %.0f/%.0f MB, load %.2f on %d, pressure %d, disk %.1f GB",
		m.TotalMB, m.SwapUsedMB, m.SwapTotalMB, m.Load1, m.Cores, m.PressureLevel, m.DiskFreeGB)
}

func TestRunKillsTheGroupOnTimeout(t *testing.T) {
	_, err := Run(300_000_000, "sh", "-c", "sleep 30 & sleep 30")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected a timeout, got %v", err)
	}
	if out, _ := exec.Command("pgrep", "-f", "^sleep 30$").Output(); len(out) > 0 {
		t.Errorf("children of a timed-out command survived: %s", out)
	}
}
