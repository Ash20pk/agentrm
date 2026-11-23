package sys

import (
	"os"
	"strconv"
	"syscall"
)

// ProcUID is the user a process runs as: the owner of its /proc entry. ok is false when the
// process is gone or unreadable.
func ProcUID(pid int) (uid int, ok bool) {
	fi, err := os.Stat("/proc/" + strconv.Itoa(pid))
	if err != nil {
		return 0, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
