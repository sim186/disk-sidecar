package scan

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"github.com/sim186/disk-sidecar/internal/idx"
)

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func indexed(t *testing.T, db string) (paths []string, texts int) {
	t.Helper()
	d, err := idx.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	rows, err := d.Query(`SELECT path FROM files ORDER BY path`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var p string
		rows.Scan(&p)
		paths = append(paths, p)
	}
	rows.Close()
	d.QueryRow(`SELECT COUNT(*) FROM file_text`).Scan(&texts)
	return paths, texts
}

func tree(t *testing.T, root string) {
	os.MkdirAll(filepath.Join(root, ".sidecar"), 0o755)
	for _, rel := range []string{"notes.txt", "code/app/main.go", "code/app/node_modules/x/index.js",
		"code/app/.git/HEAD", "code/py/venv/lib/site.py", "code/py/__pycache__/a.pyc", "Library/Caches/c.txt", ".config/a.txt"} {
		write(t, root, rel, "hello")
	}
}

// The home folder: libraries, caches and hidden folders are left out at any depth, and no text is
// read unless asked for.
func TestHomeFolder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	tree(t, home)
	db := filepath.Join(home, ".sidecar", "index.db")
	if _, err := Run(Options{Root: home, DB: db, Label: "This computer", HashMin: -1}); err != nil {
		t.Fatal(err)
	}
	paths, texts := indexed(t, db)
	want := []string{"code/app/main.go", "notes.txt"}
	sort.Strings(want)
	if len(paths) != len(want) || paths[0] != want[0] || paths[1] != want[1] {
		t.Fatalf("indexed %v, want %v", paths, want)
	}
	if texts != 0 {
		t.Fatalf("read the text of %d files, want none", texts)
	}
}

// Any other folder keeps everything but the hidden system folders, and its text is read.
func TestOtherFolder(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	disk := t.TempDir()
	tree(t, disk)
	db := filepath.Join(disk, ".sidecar", "index.db")
	if _, err := Run(Options{Root: disk, DB: db, Label: "Disk", HashMin: -1, Text: true}); err != nil {
		t.Fatal(err)
	}
	paths, texts := indexed(t, db)
	if len(paths) != 8 {
		t.Fatalf("indexed %v, want all 8 files", paths)
	}
	if texts == 0 {
		t.Fatal("no text was read")
	}
}

// The scan's lowered priority stays on its own thread.
func TestPriorityDoesNotLeak(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("checked on Linux")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	before := nice(t)
	disk := t.TempDir()
	write(t, disk, "a.txt", "x")
	os.MkdirAll(filepath.Join(disk, ".sidecar"), 0o755)
	if _, err := Run(Options{Root: disk, DB: filepath.Join(disk, ".sidecar", "index.db"), Label: "Disk", HashMin: -1}); err != nil {
		t.Fatal(err)
	}
	if after := nice(t); after != before {
		t.Fatalf("priority of the caller changed from %d to %d", before, after)
	}
}
