package farewell

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sim186/disk-sidecar/internal/catalog"
	"github.com/sim186/disk-sidecar/internal/idx"
	"github.com/sim186/disk-sidecar/internal/scan"
)

type disks struct {
	t      *testing.T
	mounts string
	A, B   string
}

func write(t *testing.T, p, body string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// setup makes two scanned disks, "Disk A" and "Disk B", that share one file.
func setup(t *testing.T) *disks {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	d := &disks{t: t, mounts: t.TempDir()}
	d.A, d.B = filepath.Join(d.mounts, "A"), filepath.Join(d.mounts, "B")
	write(t, d.A+"/foto/shared.bin", "same bytes")
	write(t, d.A+"/foto/onlyA.bin", "only on A")
	write(t, d.A+"/doc/note.txt", "ciao")
	write(t, d.A+"/doc/sub/deep.txt", "deep")
	write(t, d.B+"/backup/shared.bin", "same bytes")
	d.scan(d.A, "Disk A")
	d.scan(d.B, "Disk B")
	return d
}

func (d *disks) scan(root, label string) {
	d.t.Helper()
	os.MkdirAll(filepath.Join(root, ".sidecar"), 0o755) // the installer makes it
	if _, err := scan.Run(scan.Options{Root: root, DB: filepath.Join(root, ".sidecar", "index.db"), Label: label, HashMin: 0}); err != nil {
		d.t.Fatal(err)
	}
}

func (d *disks) box() Box {
	return Box{Catalog: catalog.Load(catalog.Options{Mounts: []string{d.mounts}, NoDefaultMounts: true, HomeDB: "/nonexistent/index.db"})}
}

func (d *disks) count(root, q string, args ...any) int {
	d.t.Helper()
	db, err := idx.OpenRO(filepath.Join(root, ".sidecar", "index.db"))
	if err != nil {
		d.t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		d.t.Fatal(err)
	}
	return n
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func refusedWith(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("want a refusal with %q, got %v", want, err)
	}
}

func TestLetGoRestoreAndEmpty(t *testing.T) {
	d := setup(t)
	res, err := d.box().LetGo([]Item{{"Disk A", "doc"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Moved != 1 || res.Files != 2 || res.Bytes != int64(len("ciao")+len("deep")) {
		t.Fatalf("result: %+v", res)
	}
	if exists(d.A+"/doc") || !exists(filepath.Join(d.A, Dir, res.Batch, "Disk A", "doc", "sub", "deep.txt")) {
		t.Fatal("folder not moved into the box")
	}
	if n := d.count(d.A, `SELECT COUNT(*) FROM files WHERE path LIKE 'doc/%'`); n != 0 {
		t.Fatalf("moved files still in the catalog: %d", n)
	}
	if !exists(d.A+"/.sidecar/index.db.bak") || !exists(d.A+"/.sidecar/log.csv") {
		t.Fatal("no backup or log")
	}

	waiting := d.box().Contents()
	if len(waiting) != 1 || waiting[0].Source != "Disk A" || waiting[0].Path != "doc" || waiting[0].IsDir != 1 {
		t.Fatalf("contents: %+v", waiting)
	}
	if _, err := d.box().Restore(waiting[0].ID); err != nil {
		t.Fatal(err)
	}
	if !exists(d.A+"/doc/sub/deep.txt") || d.count(d.A, `SELECT COUNT(*) FROM files WHERE path LIKE 'doc/%'`) != 2 {
		t.Fatal("not restored")
	}
	if len(d.box().Contents()) != 0 {
		t.Fatal("restored item still waits")
	}

	res, err = d.box().LetGo([]Item{{"Disk A", "foto/onlyA.bin"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.box().Empty("delete")
	refusedWith(t, err, "confirmation")
	gone, err := d.box().Empty("  Let It Go ")
	if err != nil || gone.Deleted != 1 {
		t.Fatalf("empty: %+v %v", gone, err)
	}
	if exists(filepath.Join(d.A, Dir, res.Batch)) {
		t.Fatal("emptied batch folder left behind")
	}
}

func TestNeverTheLastCopy(t *testing.T) {
	d := setup(t)
	_, err := d.box().LetGo([]Item{{"Disk A", "foto/shared.bin"}, {"Disk B", "backup/shared.bin"}})
	refusedWith(t, err, "ALL copies")
	if !exists(d.A+"/foto/shared.bin") || !exists(d.B+"/backup/shared.bin") {
		t.Fatal("a refused move moved something")
	}
	if _, err := d.box().LetGo([]Item{{"Disk A", "foto/shared.bin"}}); err != nil {
		t.Fatalf("one of two copies: %v", err)
	}
}

func TestRefusesWhatChangedSinceTheScan(t *testing.T) {
	d := setup(t)
	time.Sleep(1100 * time.Millisecond) // modification times are kept in seconds
	write(t, d.A+"/foto/onlyA.bin", "changed!")
	_, err := d.box().LetGo([]Item{{"Disk A", "foto/onlyA.bin"}})
	refusedWith(t, err, "modified since the last scan")

	write(t, d.A+"/doc/new.txt", "new")
	_, err = d.box().LetGo([]Item{{"Disk A", "doc"}})
	refusedWith(t, err, "folder changed since the last scan")
	if !exists(d.A + "/doc/note.txt") {
		t.Fatal("moved anyway")
	}
}

func TestRefusals(t *testing.T) {
	d := setup(t)
	os.Symlink(d.A+"/doc", d.A+"/link")
	for what, c := range map[string]struct {
		item Item
		want string
	}{
		"unknown": {Item{"Nope", "a"}, "unknown source"},
		"escape":  {Item{"Disk A", "../B/backup"}, "invalid path"},
		"index":   {Item{"Disk A", ".sidecar/index.db"}, "invalid path"},
		"missing": {Item{"Disk A", "nothing.txt"}, "does not exist"},
		"symlink": {Item{"Disk A", "link"}, "symbolic link"},
	} {
		_, err := d.box().LetGo([]Item{c.item})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", what, err)
		}
	}
}

func TestGuardIsAsked(t *testing.T) {
	d := setup(t)
	b := d.box()
	var asked []string
	b.Guard = func(full string) error { asked = append(asked, full); return errors.New("not here") }
	_, err := b.LetGo([]Item{{"Disk A", "foto/onlyA.bin"}})
	refusedWith(t, err, "not here")
	if len(asked) != 1 || asked[0] != filepath.Join(d.A, "foto", "onlyA.bin") {
		t.Fatalf("guard asked %v", asked)
	}
}

func TestCheckDropsItemsInsideAnotherAndRepeats(t *testing.T) {
	d := setup(t)
	sel, err := d.box().Check([]Item{{"Disk A", "doc/sub/deep.txt"}, {"Disk A", "doc"}, {"Disk A", "doc/"}}, "moved")
	if err != nil {
		t.Fatal(err)
	}
	defer sel.Close()
	if len(sel.Picks) != 1 || sel.Picks[0].Rel != "doc" || len(sel.Picks[0].Files) != 2 {
		t.Fatalf("picks: %+v", sel.Picks)
	}
}

func TestRestoreNeverOverwrites(t *testing.T) {
	d := setup(t)
	if _, err := d.box().LetGo([]Item{{"Disk A", "foto/onlyA.bin"}}); err != nil {
		t.Fatal(err)
	}
	write(t, d.A+"/foto/onlyA.bin", "someone else")
	_, err := d.box().Restore(d.box().Contents()[0].ID)
	refusedWith(t, err, "not overwriting")
	if b, _ := os.ReadFile(d.A + "/foto/onlyA.bin"); string(b) != "someone else" {
		t.Fatal("overwritten")
	}
}
