package sys

import "golang.org/x/sys/unix"

// ProcUID is the user a process runs as (its effective UID), from the kernel. ok is false when
// the process is gone or unreadable.
func ProcUID(pid int) (uid int, ok bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp.Proc.P_pid == 0 {
		return 0, false
	}
	return int(kp.Eproc.Ucred.Uid), true
}
