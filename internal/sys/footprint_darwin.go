package sys

import (
	"encoding/binary"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Footprint is a process's physical footprint in MB: what Activity Monitor shows as its
// memory, compressed pages included. ps's RSS leaves those out, so an idle process macOS has
// compressed looks small to ps while it still holds its memory. ok is false if unreadable.
func Footprint(pid int) (float64, bool) {
	// proc_pid_rusage(pid, RUSAGE_INFO_V0, &info): rusage_info_v0 is a 16-byte UUID followed
	// by ten uint64s; phys_footprint is the eighth of them.
	const (
		procInfoCallPidRusage = 9
		rusageInfoV0          = 0
		footprintOffset       = 16 + 7*8
	)
	var buf [16 + 10*8]byte
	_, _, errno := syscall.Syscall6(unix.SYS_PROC_INFO, procInfoCallPidRusage, uintptr(pid), rusageInfoV0, 0,
		uintptr(unsafe.Pointer(&buf[0])), 0)
	if errno != 0 {
		return 0, false
	}
	return float64(binary.LittleEndian.Uint64(buf[footprintOffset:])) / (1 << 20), true
}
