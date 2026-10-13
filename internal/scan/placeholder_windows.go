package scan

import (
	"io/fs"
	"syscall"
)

// Placeholder tells whether a file's contents are only in the cloud (OneDrive "Files On-Demand"
// and other cloud files): reading it would download it.
func Placeholder(info fs.FileInfo) bool {
	const offline, recallOnOpen, recallOnDataAccess = 0x1000, 0x40000, 0x400000
	a, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok && a.FileAttributes&(offline|recallOnOpen|recallOnDataAccess) != 0
}
