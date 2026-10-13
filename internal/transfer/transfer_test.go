package transfer

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sim186/disk-sidecar/internal/catalog"
	"github.com/sim186/disk-sidecar/internal/farewell"
	"github.com/sim186/disk-sidecar/internal/idx"
	"github.com/sim186/disk-sidecar/internal/scan"
)

func write(t *testing.T, p, body string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

type world struct {
	t            *testing.T
	mounts, A, B string
}

// setup makes two scanned disks: "Disk A" with a file and a folder of text, and an empty "Disk B".
func setup(t *testing.T) *world {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	w := &world{t: t, mounts: t.TempDir()}
	w.A, w.B = filepath.Join(w.mounts, "A"), filepath.Join(w.mounts, "B")
	write(t, w.A+"/foto/a.bin", "photo bytes")
	write(t, w.A+"/doc/note.txt", "ciao mondo")
	write(t, w.A+"/doc/sub/deep.txt", "deep")
	os.MkdirAll(w.B, 0o755)
	for root, label := range map[string]string{w.A: "Disk A", w.B: "Disk B"} {
		os.MkdirAll(filepath.Join(root, ".sidecar"), 0o755)
		if _, err := scan.Run(scan.Options{Root: root, DB: filepath.Join(root, ".sidecar", "index.db"), Label: label, HashMin: -1, Text: true}); err != nil {
			t.Fatal(err)
		}
	}
	return w
}

func (w *world) catalog() *catalog.Catalog {
	return catalog.Load(catalog.Options{Mounts: []string{w.mounts}, NoDefaultMounts: true, HomeDB: "/nonexistent/index.db"})
}

// fromDisks copies items from the disks to the folder to.
func (w *world) fromDisks(to farewell.Item, items ...farewell.Item) (Copied, error) {
	cat := w.catalog()
	sel, err := farewell.Box{Catalog: cat}.Check(items, "copied")
	if err != nil {
		return Copied{}, err
	}
	defer sel.Close()
	its, from := FromDisks(sel)
	return Copier{Catalog: cat}.Copy(its, from, to)
}

func (w *world) count(root, q string, args ...any) int {
	w.t.Helper()
	db, err := idx.OpenRO(filepath.Join(root, ".sidecar", "index.db"))
	if err != nil {
		w.t.Fatal(err)
	}
	defer db.Close()
	var n int
	db.QueryRow(q, args...).Scan(&n)
	return n
}

func TestCopyBetweenDisks(t *testing.T) {
	w := setup(t)
	res, err := w.fromDisks(farewell.Item{Source: "Disk B", Path: "from-a/2026"}, farewell.Item{Source: "Disk A", Path: "foto/a.bin"}, farewell.Item{Source: "Disk A", Path: "doc"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Copied != 2 || res.Files != 3 {
		t.Fatalf("result: %+v", res)
	}
	for _, p := range []string{"from-a/2026/a.bin", "from-a/2026/doc/note.txt", "from-a/2026/doc/sub/deep.txt"} {
		if !exists(filepath.Join(w.B, p)) || exists(filepath.Join(w.B, p)+PartSuffix) {
			t.Fatalf("%s: missing, or its temporary file left behind", p)
		}
	}
	a, _ := os.Stat(w.A + "/foto/a.bin")
	b, _ := os.Stat(w.B + "/from-a/2026/a.bin")
	if a.ModTime().Unix() != b.ModTime().Unix() {
		t.Fatal("modification time not kept")
	}
	if n := w.count(w.B, `SELECT COUNT(*) FROM files WHERE sha256 IS NOT NULL AND top='from-a'`); n != 3 {
		t.Fatalf("copies indexed with their hash: %d", n)
	}
	if n := w.count(w.B, `SELECT COUNT(*) FROM file_text WHERE file_text MATCH 'mondo'`); n != 1 {
		t.Fatal("text did not come with the copy")
	}
	if n := w.count(w.A, `SELECT COUNT(*) FROM files WHERE sha256 IS NOT NULL`); n != 3 {
		t.Fatalf("originals did not learn their hash: %d", n)
	}
}

func TestCopyRefusals(t *testing.T) {
	w := setup(t)
	write(t, w.B+"/taken/a.bin", "already here")
	if _, err := scan.Run(scan.Options{Root: w.B, DB: filepath.Join(w.B, ".sidecar", "index.db"), Label: "Disk B", HashMin: -1}); err != nil {
		t.Fatal(err)
	}
	os.Symlink(w.B+"/taken", w.B+"/link")
	A := func(p string) farewell.Item { return farewell.Item{Source: "Disk A", Path: p} }
	B := func(p string) farewell.Item { return farewell.Item{Source: "Disk B", Path: p} }
	for what, c := range map[string]struct {
		to    farewell.Item
		items []farewell.Item
		want  string
	}{
		"overwrite":   {B("taken"), []farewell.Item{A("foto/a.bin")}, "not overwriting"},
		"into itself": {A("doc/inner"), []farewell.Item{A("doc")}, "into itself"},
		"same name":   {B("x"), []farewell.Item{A("foto/a.bin"), B("taken/a.bin")}, "both called"},
		"unknown":     {farewell.Item{Source: "Nope"}, []farewell.Item{A("doc")}, "unknown source"},
		"bad path":    {B("../x"), []farewell.Item{A("doc")}, "invalid path"},
		"index":       {B(".sidecar/x"), []farewell.Item{A("doc")}, "invalid path"},
		"symlink":     {B("link"), []farewell.Item{A("doc")}, "symbolic link"},
		"a file":      {B("taken/a.bin"), []farewell.Item{A("doc")}, "not a folder"},
	} {
		if _, err := w.fromDisks(c.to, c.items...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", what, err)
		}
	}
	if exists(w.A + "/doc/inner") {
		t.Fatal("folder copied into itself")
	}
}

// fakePhone is a phone whose storage is a folder; lie makes it report wrong hashes.
type fakePhone struct {
	dir string
	lie string // a path whose hash the phone reports wrongly
}

func (f *fakePhone) origin(db *sql.DB) *Phone {
	return &Phone{DB: db, Serial: "SER1",
		Pull: func(serial, remote, local string) error {
			b, err := os.ReadFile(filepath.Join(f.dir, strings.TrimPrefix(remote, "/sdcard/")))
			if err != nil {
				return err
			}
			return os.WriteFile(local, b, 0o644)
		},
		SHA: func(serial, remote string) (string, error) {
			rel := strings.TrimPrefix(remote, "/sdcard/")
			if rel == f.lie {
				return "0000", nil
			}
			return scan.SHA256(filepath.Join(f.dir, rel))
		}}
}

// phoneCatalog catalogs the fake phone like `sidecar phone scan` does.
func phoneCatalog(t *testing.T, dir string) (*sql.DB, *catalog.Source) {
	db, err := idx.Open(filepath.Join(t.TempDir(), "phone.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	sid, _ := idx.EnsureSource(db, "Pixel", "/sdcard", "")
	tx, _ := db.Begin()
	fw, _ := idx.NewFileWriter(tx, sid)
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			rel, _ := filepath.Rel(dir, p)
			fw.Put(filepath.ToSlash(rel), info.Size(), info.ModTime().Unix(), sql.NullString{})
		}
		return nil
	})
	fw.Close()
	tx.Commit()
	return db, &catalog.Source{Label: "Pixel", SID: sid, Kind: "phone"}
}

func TestCopyFromAPhoneWithoutAdb(t *testing.T) {
	w := setup(t)
	ph := &fakePhone{dir: t.TempDir()}
	write(t, ph.dir+"/DCIM/a.jpg", "jpeg a")
	write(t, ph.dir+"/DCIM/b.jpg", "jpeg b")
	db, src := phoneCatalog(t, ph.dir)

	items, err := PhoneItems(db, src, []farewell.Item{{Source: "Pixel", Path: "DCIM"}, {Source: "Pixel", Path: "DCIM/a.jpg"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || !items[0].IsDir || len(items[0].Files) != 2 {
		t.Fatalf("a file chosen twice: %+v", items)
	}
	res, err := Copier{Catalog: w.catalog()}.Copy(items, ph.origin(db), farewell.Item{Source: "Disk B", Path: "phone"})
	if err != nil || res.Files != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	if b, _ := os.ReadFile(w.B + "/phone/DCIM/b.jpg"); string(b) != "jpeg b" {
		t.Fatal("copy differs")
	}
	var hashed int
	db.QueryRow(`SELECT COUNT(*) FROM files WHERE sha256 IS NOT NULL`).Scan(&hashed)
	if hashed != 2 {
		t.Fatalf("the phone's catalog did not learn the proven hashes: %d", hashed)
	}
	if !strings.Contains(readLog(t, w.B), "phone/DCIM (from Pixel)") {
		t.Fatal("copy not logged on the destination disk")
	}
}

func TestAPhoneThatDisagreesKeepsNothing(t *testing.T) {
	w := setup(t)
	ph := &fakePhone{dir: t.TempDir(), lie: "DCIM/b.jpg"}
	write(t, ph.dir+"/DCIM/a.jpg", "jpeg a")
	write(t, ph.dir+"/DCIM/b.jpg", "jpeg b")
	db, src := phoneCatalog(t, ph.dir)
	items, _ := PhoneItems(db, src, []farewell.Item{{Source: "Pixel", Path: "DCIM"}})
	_, err := Copier{Catalog: w.catalog()}.Copy(items, ph.origin(db), farewell.Item{Source: "Disk B", Path: ""})
	if err == nil || !strings.Contains(err.Error(), "does not match what the phone holds") || !strings.Contains(err.Error(), "Nothing of it was kept") {
		t.Fatalf("%v", err)
	}
	if exists(w.B + "/DCIM") {
		t.Fatal("part of a failed copy was kept")
	}
	if n := w.count(w.B, `SELECT COUNT(*) FROM files`); n != 0 {
		t.Fatal("a failed copy reached the index")
	}
}

func TestTheGuardIsAskedForTheDestination(t *testing.T) {
	w := setup(t)
	cat := w.catalog()
	sel, _ := farewell.Box{Catalog: cat}.Check([]farewell.Item{{Source: "Disk A", Path: "foto/a.bin"}}, "copied")
	defer sel.Close()
	items, from := FromDisks(sel)
	_, err := Copier{Catalog: cat, Guard: func(string) error { return errors.New("system disk") }}.Copy(items, from, farewell.Item{Source: "Disk B", Path: "x"})
	if err == nil || !strings.Contains(err.Error(), "system disk") || exists(w.B+"/x") {
		t.Fatalf("%v", err)
	}
}

func readLog(t *testing.T, root string) string {
	b, _ := os.ReadFile(filepath.Join(root, ".sidecar", "log.csv"))
	return string(b)
}
