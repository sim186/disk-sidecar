package server

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// The system disk is not a disk like the others: it holds the operating system and the programs.
// Sidecar may be installed there, but it only touches the user's own files on it. Everything that
// changes files (move to the farewell box, restore, copy) asks the guard first.

type guardEnv struct {
	windows  bool
	home     string   // the user's home folder, links resolved
	sysDrive string   // Windows: the drive Windows runs from, "C:"
	external []string // Mac and Linux: folders where removable disks are mounted
}

func currentGuard() guardEnv {
	g := guardEnv{windows: runtime.GOOS == "windows", external: []string{"/Volumes", "/media", "/mnt", "/run/media"}}
	if h, err := os.UserHomeDir(); err == nil {
		g.home = realpath(h)
	}
	if g.windows {
		g.sysDrive = os.Getenv("SystemDrive")
		if g.sysDrive == "" {
			g.sysDrive = "C:"
		}
	}
	return g
}

// guardPath is the check used by the server for a real path on this computer.
func (s *Server) guardPath(p string) error {
	anc, below := existingAncestor(p) // the path may not exist (yet): resolve the part that does
	return currentGuard().check(filepath.Join(realpath(anc), below))
}

var errNotUser = errors.New("this is on the system disk, outside your user folders. Sidecar only changes your own files there (Documents, Pictures, …), never the system or programs")

// check returns nil if p may be changed. p must have its links resolved.
func (g guardEnv) check(p string) error {
	if g.windows {
		return g.checkWindows(p)
	}
	for _, b := range g.external {
		if strings.HasPrefix(p, b+"/") {
			return nil // a disk that is plugged in, not the system disk
		}
	}
	rest, ok := underHome(p, g.home, false)
	if !ok {
		// other users' folders and a home folder somewhere else follow the same pattern
		parts := strings.Split(strings.Trim(p, "/"), "/")
		if len(parts) >= 2 && (parts[0] == "Users" || parts[0] == "home") {
			rest, ok = parts[2:], true
		}
	}
	if !ok {
		return errNotUser
	}
	return homeRest(rest, false)
}

func (g guardEnv) checkWindows(p string) error {
	p = strings.ReplaceAll(p, `\`, "/")
	if len(p) < 2 || p[1] != ':' || !strings.EqualFold(p[:2], g.sysDrive) {
		return nil // another drive, or a network share
	}
	home := strings.ReplaceAll(g.home, `\`, "/")
	rest, ok := underHome(p, home, true)
	if !ok {
		parts := strings.Split(strings.Trim(p[2:], "/"), "/")
		if len(parts) >= 2 && strings.EqualFold(parts[0], "Users") {
			rest, ok = parts[2:], true
		}
	}
	if !ok {
		return errNotUser
	}
	return homeRest(rest, true)
}

// underHome splits p into the parts below home.
func underHome(p, home string, fold bool) ([]string, bool) {
	if home == "" {
		return nil, false
	}
	h := strings.TrimRight(home, "/")
	pp := p
	if fold {
		h, pp = strings.ToLower(h), strings.ToLower(p)
	}
	if pp != h && !strings.HasPrefix(pp, h+"/") {
		return nil, false
	}
	tail := strings.Trim(p[len(h):], "/")
	if tail == "" {
		return nil, true
	}
	return strings.Split(tail, "/"), true
}

// homeRest decides on the part below a home folder: never the home itself, never the folders
// where programs keep their own data.
func homeRest(rest []string, windows bool) error {
	if len(rest) == 0 {
		return errors.New("this is a whole home folder. Choose a folder or file inside it")
	}
	first := rest[0]
	lower := strings.ToLower(first)
	if first == "Library" || strings.HasPrefix(first, ".") || (windows && (lower == "appdata" || strings.HasPrefix(lower, "ntuser"))) {
		return errors.New("this is where programs keep their own data. Sidecar leaves it alone")
	}
	return nil
}
