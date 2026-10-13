package scan

import "syscall"

// background puts the calling thread in the background band: macOS then throttles its disk
// reads and gives it the CPU last, so the computer stays responsive while it scans.
func background() {
	const prioDarwinThread, prioDarwinBG = 3, 0x1000
	syscall.Setpriority(prioDarwinThread, 0, prioDarwinBG)
}
