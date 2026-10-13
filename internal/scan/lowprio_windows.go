package scan

import "syscall"

var setThreadPriority = syscall.NewLazyDLL("kernel32.dll").NewProc("SetThreadPriority")

// background puts the calling thread in background mode: Windows then lowers its CPU, disk and
// memory priority, so the computer stays responsive while it scans.
func background() {
	const currentThread = ^uintptr(1) // the pseudo handle GetCurrentThread returns, (HANDLE)-2
	const threadModeBackgroundBegin = 0x00010000
	setThreadPriority.Call(currentThread, threadModeBackgroundBegin)
}
