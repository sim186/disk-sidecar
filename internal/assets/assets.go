// Package assets holds everything that is compiled into the binary: the web page and its
// libraries, the version, and the small files the installer writes next to the index.
package assets

import (
	"embed"
	"io/fs"
	"strings"
)

//go:embed web disk VERSION
var files embed.FS

// Version is the program version, from the VERSION file.
var Version = func() string {
	b, _ := files.ReadFile("VERSION")
	return strings.TrimSpace(string(b))
}()

// Web is the content served to the browser: catalog.html and vendor/.
func Web() fs.FS { sub, _ := fs.Sub(files, "web"); return sub }

// Disk is the set of files the installer puts in <disk>/.sidecar/ besides the binaries.
func Disk() fs.FS { sub, _ := fs.Sub(files, "disk"); return sub }

// Page returns catalog.html.
func Page() []byte { b, _ := files.ReadFile("web/catalog.html"); return b }
