package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/sim186/disk-sidecar/internal/catalog"
)

// diskFile resolves a source label and a path relative to its root to a file that is on a disk
// plugged in now. Nothing outside the disk is ever returned.
func (s *Server) diskFile(source, rel string) (string, os.FileInfo, error) {
	rel, err := catalog.CleanRel(rel)
	if err != nil {
		return "", nil, err
	}
	src, err := s.recentCatalog().Writable(source, "its files are not here to show")
	if err != nil {
		return "", nil, err
	}
	if !src.Reachable() {
		return "", nil, refuse("%s is not plugged in", src.Label)
	}
	full := filepath.Join(src.Root, filepath.FromSlash(rel))
	st, err := os.Lstat(full)
	if err != nil {
		return "", nil, refuse("the file is no longer on the disk: %s", rel)
	}
	if !catalog.Inside(src.Root, full) {
		return "", nil, refuse("invalid path: %q", rel)
	}
	return full, st, nil
}

// doReveal shows a file in Finder / Explorer / the file manager. The page sends the label of the
// disk and a path relative to its root, like for marks; nothing outside the disk is ever shown.
func (s *Server) doReveal(source, rel string) error {
	full, st, err := s.diskFile(source, rel)
	if err != nil {
		return err
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", "-R", full)
	case "windows":
		cmd = exec.Command("explorer", "/select,"+full) // explorer exits with 1 even on success
	default: // no portable "select": open the folder
		dir := full
		if !st.IsDir() {
			dir = filepath.Dir(full)
		}
		cmd = exec.Command("xdg-open", dir)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}
