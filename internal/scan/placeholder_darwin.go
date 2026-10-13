package scan

import (
	"io/fs"
	"syscall"
)

// Placeholder tells whether a file's contents are only in the cloud (iCloud Drive with "Optimise
// Mac Storage", or another File Provider): reading it would download it.
func Placeholder(info fs.FileInfo) bool {
	const sfDataless = 0x40000000
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Flags&sfDataless != 0
}
