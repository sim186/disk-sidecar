package catalog

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/sim186/disk-sidecar/internal/refusal"
)

// CleanRel validates a path sent by the page: relative, no "..", never inside .sidecar.
func CleanRel(rel string) (string, error) {
	rel = strings.Trim(strings.ReplaceAll(rel, "\\", "/"), "/")
	parts := strings.Split(rel, "/")
	bad := rel == ""
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || p == ".sidecar" {
			bad = true
		}
	}
	if !bad && strings.Contains(parts[0], ":") {
		bad = true
	}
	if bad {
		return "", refusal.New("invalid path: %q", rel)
	}
	return rel, nil
}

// Realpath resolves links. For a path that does not exist (yet), the part that does is resolved,
// so /var/x/missing on macOS becomes /private/var/x/missing like its folder.
func Realpath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	a, _ := filepath.Abs(p)
	anc, below := ExistingAncestor(a)
	if r, err := filepath.EvalSymlinks(anc); err == nil {
		return filepath.Join(r, below)
	}
	return a
}

// Inside reports whether full is strictly inside root, after resolving links.
func Inside(root, full string) bool {
	r, f := Realpath(root), Realpath(full)
	rel, err := filepath.Rel(r, f)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ExistingAncestor returns the deepest part of full that exists, and the part below it.
func ExistingAncestor(full string) (string, string) {
	rest := ""
	for p := full; ; p = filepath.Dir(p) {
		if _, err := os.Lstat(p); err == nil {
			return p, rest
		}
		if filepath.Dir(p) == p {
			return p, rest
		}
		rest = filepath.Join(filepath.Base(p), rest)
	}
}

// DirExists reports whether p is a folder.
func DirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
