package scan

import "syscall"

// background lowers the calling thread's CPU and disk priority (nice 10, lowest best-effort I/O).
func background() {
	tid := syscall.Gettid()
	syscall.Setpriority(syscall.PRIO_PROCESS, tid, 10)
	const whoProcess, classBestEffort = 1, 2
	syscall.Syscall(syscall.SYS_IOPRIO_SET, whoProcess, uintptr(tid), classBestEffort<<13|7)
}
