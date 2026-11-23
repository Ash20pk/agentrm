package sys

import "golang.org/x/sys/unix"

// ProcStartID is when process pid started, to the microsecond, from the kernel: with the PID,
// it names one process, where a PID alone may since have been given to another.
func ProcStartID(pid int) (uint64, bool) {
	if pid <= 0 {
		return 0, false
	}
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp.Proc.P_pid != int32(pid) {
		return 0, false
	}
	t := kp.Proc.P_starttime
	return uint64(t.Sec)*1_000_000 + uint64(t.Usec), true
}
