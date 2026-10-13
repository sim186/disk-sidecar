package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sim186/disk-sidecar/internal/idx"
)

// disk makes a mounted disk under mounts with one source called label.
func disk(t *testing.T, mounts, name, label string) string {
	t.Helper()
	root := filepath.Join(mounts, name)
	os.MkdirAll(filepath.Join(root, ".sidecar"), 0o755)
	db, err := idx.Open(filepath.Join(root, ".sidecar", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := idx.EnsureSource(db, label, root, ""); err != nil {
		t.Fatal(err)
	}
	return root
}

func load(t *testing.T) (*Catalog, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // no phones from the real computer
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	mounts := t.TempDir()
	a := disk(t, mounts, "A", "Photos")
	disk(t, mounts, "B", "Photos")
	os.MkdirAll(filepath.Join(a, ".sidecar", "peers"), 0o755)
	peer, _ := idx.Open(filepath.Join(a, ".sidecar", "peers", "laptop.db"))
	idx.EnsureSource(peer, "Laptop", "/Users/me", "laptop")
	peer.Close()
	return Load(Options{Mounts: []string{mounts}, NoDefaultMounts: true, HomeDB: "/nonexistent/index.db"}), a
}

func TestLabelsAreUniqueAcrossDisks(t *testing.T) {
	c, _ := load(t)
	var labels []string
	for _, x := range c.Sources() {
		labels = append(labels, x.Label+"="+x.Kind)
	}
	if got := strings.Join(labels, ","); got != "Photos=disk,Photos (B)=disk,Laptop=snapshot" {
		t.Fatalf("labels: %s", got)
	}
	if len(c.Disks()) != 2 || len(c.Indexes()) != 3 {
		t.Fatalf("disks %d, indexes %d", len(c.Disks()), len(c.Indexes()))
	}
	b := c.Find("Photos (B)")
	if b == nil || b.Raw != "Photos" || c.Source(b.DB, b.SID) != b || c.Disk(b.Disk.Key) != b.Disk || len(c.In(b.DB)) != 1 {
		t.Fatalf("lookups disagree: %+v", b)
	}
	if !b.Reachable() || c.Find("Laptop").Reachable() {
		t.Fatal("reachability")
	}
}

func TestWritableRefusesWhatSidecarOnlyReads(t *testing.T) {
	c, _ := load(t)
	if _, err := c.Writable("Photos", "its files cannot be moved"); err != nil {
		t.Fatal(err)
	}
	_, err := c.Writable("Laptop", "its files cannot be moved")
	if err == nil || err.Error() != "Laptop is a read-only snapshot: its files cannot be moved" {
		t.Fatalf("snapshot: %v", err)
	}
	if _, err := c.Writable("Nope", "x"); err == nil || !strings.Contains(err.Error(), "unknown source") {
		t.Fatalf("unknown: %v", err)
	}
}

func TestCleanRel(t *testing.T) {
	for in, want := range map[string]string{"a/b": "a/b", "/a\\b/": "a/b", "": "", "../x": "", "a/../b": "", ".sidecar/x": "", "C:/x": ""} {
		got, err := CleanRel(in)
		if got != want || (want == "") != (err != nil) {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
}

func TestInsideThroughALinkedFolder(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link") // like /var -> /private/var on macOS
	if err := os.Symlink(real, link); err != nil {
		t.Skip("no symbolic links here")
	}
	os.MkdirAll(filepath.Join(real, "disk"), 0o755)
	root := filepath.Join(link, "disk")
	if !Inside(root, filepath.Join(root, "missing.txt")) || !Inside(root, filepath.Join(root, "new", "dir")) {
		t.Fatal("a path that does not exist yet counted as outside its disk")
	}
	if Inside(root, filepath.Join(root, "..", "other")) {
		t.Fatal("escaped the disk")
	}
}
