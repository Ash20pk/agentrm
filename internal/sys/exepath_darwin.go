package sys

import (
	"bytes"
	"syscall"
	"unsafe"
)

// proc_info(2) call and flavor numbers, from <sys/proc_info.h>.
const (
	procInfoCallPIDInfo = 2
	procPIDPathInfo     = 11
	procPIDPathMax      = 4 * 1024 // PROC_PIDPATHINFO_MAXSIZE
)

// ExePath returns the real path of the executable pid runs, from the kernel: what libproc's
// proc_pidpath returns, read with the proc_info system call so that no cgo is needed. The kernel
// takes it from the executable's vnode, so unlike argv[0] (or the exec path in
// kern.procargs2, which lives in the process's own memory) the process cannot choose it.
// "" when the process is gone or not readable.
func ExePath(pid int) string {
	if pid <= 0 {
		return ""
	}
	var buf [procPIDPathMax]byte
	_, _, e := syscall.Syscall6(syscall.SYS_PROC_INFO, procInfoCallPIDInfo, uintptr(pid), procPIDPathInfo, 0,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if e != 0 {
		return ""
	}
	b := buf[:]
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}
