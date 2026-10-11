package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// doReveal shows a file in Finder / Explorer / the file manager. The page sends the label of the
// disk and a path relative to its root, like for marks; nothing outside the disk is ever shown.
func (s *Server) doReveal(source, rel string) error {
	rel, err := cleanRel(rel)
	if err != nil {
		return err
	}
	var src *Src
	srcs, _ := s.listSources()
	for _, x := range srcs {
		if x.Label == source {
			src = x
		}
	}
	if src == nil {
		return refuse("unknown source: %s", source)
	}
	if src.Kind != "disk" {
		return refuse("%s is a snapshot of another computer: its files are not here", src.Label)
	}
	if !dirExists(src.Root) {
		return refuse("%s is not plugged in", src.Label)
	}
	full := filepath.Join(src.Root, filepath.FromSlash(rel))
	st, err := os.Lstat(full)
	if err != nil {
		return refuse("the file is no longer on the disk: %s", rel)
	}
	if !inside(src.Root, full) {
		return refuse("invalid path: %q", rel)
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
