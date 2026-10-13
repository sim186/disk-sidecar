package idx

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func open(t *testing.T) *sql.DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func count(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestNameParts(t *testing.T) {
	for rel, want := range map[string][3]string{
		"a.JPG":           {"a.JPG", "jpg", ""},
		"foto/2026/b.png": {"b.png", "png", "foto"},
		"home/.bashrc":    {".bashrc", "", "home"},
		"noext":           {"noext", "", ""},
	} {
		n, e, top := NameParts(rel)
		if [3]string{n, e, top} != want {
			t.Errorf("%s: %q %q %q, want %v", rel, n, e, top, want)
		}
	}
}

func put(t *testing.T, db *sql.DB, sid int64, rel, text string) int64 {
	t.Helper()
	tx, _ := db.Begin()
	w, err := NewFileWriter(tx, sid)
	if err != nil {
		t.Fatal(err)
	}
	id, err := w.Put(rel, 10, 100, sql.NullString{})
	if err == nil && text != "" {
		err = w.PutText(id, rel, text)
	}
	w.Close()
	if err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	return id
}

func TestFileWriterDerivesTheRowAndUpdatesInPlace(t *testing.T) {
	db := open(t)
	sid, err := EnsureSource(db, "Disk", "/d", "host")
	if err != nil {
		t.Fatal(err)
	}
	id := put(t, db, sid, "foto/a.JPG", "hello")
	if again := put(t, db, sid, "foto/a.JPG", "world"); again != id {
		t.Fatalf("second put made a new row: %d %d", id, again)
	}
	var name, ext, kind, top string
	db.QueryRow(`SELECT name, ext, kind, top FROM files WHERE id=?`, id).Scan(&name, &ext, &kind, &top)
	if name != "a.JPG" || ext != "jpg" || kind != "photo" || top != "foto" {
		t.Fatalf("row: %s %s %s %s", name, ext, kind, top)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM file_text WHERE file_text MATCH 'world'`); n != 1 {
		t.Fatal("text not replaced")
	}
	if n := count(t, db, `SELECT COUNT(*) FROM file_text`); n != 1 {
		t.Fatal("old text kept")
	}
}

func TestMoveAndRestoreRoundTrip(t *testing.T) {
	db := open(t)
	sid, _ := EnsureSource(db, "Disk", "/d", "")
	a := put(t, db, sid, "dir/a.txt", "alpha")
	b := put(t, db, sid, "dir/b.txt", "")
	put(t, db, sid, "other.txt", "")
	mark := "delete"
	SetMark(db, sid, "dir", &mark, nil)
	SetMark(db, sid, "dir/a.txt", &mark, nil)
	SetMark(db, sid, "other.txt", &mark, nil)

	tx, _ := db.Begin()
	if err := RecordMove(tx, Move{Batch: "b1", SID: sid, Path: "dir", QPath: ".sidecar/_quarantine/b1/Disk/dir", IsDir: true, Files: 2, Size: 20, MovedAt: 1}, []int64{a, b}); err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	if n := count(t, db, `SELECT COUNT(*) FROM files`); n != 1 {
		t.Fatalf("files left: %d", n)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM marks`); n != 1 {
		t.Fatalf("marks of the moved folder kept: %d", n)
	}
	moves, _ := Moves(db)
	if len(moves) != 1 || !moves[0].IsDir || moves[0].Files != 2 || moves[0].Path != "dir" {
		t.Fatalf("moves: %+v", moves)
	}
	m, err := MoveByID(db, "1")
	if err != nil {
		t.Fatal(err)
	}
	tx, _ = db.Begin()
	if err := RecordRestore(tx, m); err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	if n := count(t, db, `SELECT COUNT(*) FROM files WHERE source_id=?`, sid); n != 3 {
		t.Fatalf("files after restore: %d", n)
	}
	if moves, _ := Moves(db); len(moves) != 0 {
		t.Fatal("restored move still waits in the box")
	}
}

func TestReplaceSourceForgetsOldFiles(t *testing.T) {
	db := open(t)
	sid, _ := EnsureSource(db, "Disk", "/d", "")
	put(t, db, sid, "a", "x")
	again, replaced, err := ReplaceSource(db, "Disk", "/e", "h", 5)
	if err != nil || !replaced || again != sid {
		t.Fatalf("%d %v %v", again, replaced, err)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM files`) + count(t, db, `SELECT COUNT(*) FROM file_text`); n != 0 {
		t.Fatal("old files kept")
	}
	if _, replaced, _ := ReplaceSource(db, "New", "/n", "", 0); replaced {
		t.Fatal("a new source counted as replaced")
	}
}

func TestSetMarkRefusesUnknownPiles(t *testing.T) {
	db := open(t)
	bad := "maybe"
	if SetMark(db, 1, "a", &bad, nil) == nil {
		t.Fatal("accepted an unknown pile")
	}
}
