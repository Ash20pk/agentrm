package sys

import (
	"sync"
	"syscall"
	"unsafe"
)

// Thermal pressure, as macOS itself rates it: the level behind ProcessInfo.thermalState and the
// fan of Activity Monitor's "thermal state". It is read from notify(3), the same channel the
// system posts it on, through libSystem with no cgo (the way golang.org/x/sys calls libc): the
// release build has cgo off, and no sysctl carries it on Apple silicon.
//
//	0 nominal · 1 moderate (fair) · 2 heavy (serious: the Mac is slowing itself down)
//	3 trapping · 4 sleeping (critical)

//go:cgo_import_dynamic libc_notify_register_check notify_register_check "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_notify_get_state notify_get_state "/usr/lib/libSystem.B.dylib"

var libc_notify_register_check_trampoline_addr uintptr
var libc_notify_get_state_trampoline_addr uintptr

//go:linkname syscall_syscall syscall.syscall
func syscall_syscall(fn, a1, a2, a3 uintptr) (r1, r2 uintptr, err syscall.Errno)

const thermalNotifyName = "com.apple.system.thermalpressurelevel"

var thermal struct {
	once  sync.Once
	token int32
	ok    bool
}

// ThermalLevel is the machine's thermal pressure now (see above), or -1 when it cannot be read.
func ThermalLevel() int {
	thermal.once.Do(func() {
		name := []byte(thermalNotifyName + "\x00")
		r, _, _ := syscall_syscall(libc_notify_register_check_trampoline_addr,
			uintptr(unsafe.Pointer(&name[0])), uintptr(unsafe.Pointer(&thermal.token)), 0)
		thermal.ok = r == 0 // NOTIFY_STATUS_OK
	})
	if !thermal.ok {
		return -1
	}
	var state uint64
	if r, _, _ := syscall_syscall(libc_notify_get_state_trampoline_addr,
		uintptr(thermal.token), uintptr(unsafe.Pointer(&state)), 0); r != 0 {
		return -1
	}
	return int(min(state, 4))
}
