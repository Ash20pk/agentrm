package sys

import (
	"os"
	"strconv"
	"strings"
)

// ProcStartID is when process pid started, in clock ticks since boot (field 22 of
// /proc/pid/stat): with the PID, it names one process.
func ProcStartID(pid int) (uint64, bool) {
	if pid <= 0 {
		return 0, false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')') // the command name may hold spaces and parentheses
	if i < 0 {
		return 0, false
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 20 {
		return 0, false
	}
	v, err := strconv.ParseUint(f[19], 10, 64)
	return v, err == nil
}
