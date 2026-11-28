// Trampolines into libSystem for ThermalLevel (thermal_darwin.go).

#include "textflag.h"

TEXT libc_notify_register_check_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_notify_register_check(SB)
GLOBL	·libc_notify_register_check_trampoline_addr(SB), RODATA, $8
DATA	·libc_notify_register_check_trampoline_addr(SB)/8, $libc_notify_register_check_trampoline<>(SB)

TEXT libc_notify_get_state_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_notify_get_state(SB)
GLOBL	·libc_notify_get_state_trampoline_addr(SB), RODATA, $8
DATA	·libc_notify_get_state_trampoline_addr(SB)/8, $libc_notify_get_state_trampoline<>(SB)
