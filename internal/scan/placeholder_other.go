//go:build !darwin && !windows

package scan

import "io/fs"

// Placeholder tells whether a file's contents are only in the cloud. Not detected on this system.
func Placeholder(info fs.FileInfo) bool { return false }
