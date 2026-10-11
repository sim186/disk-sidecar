// Package idx is everything about the index file itself: its layout, how to open it, the
// kinds of files, and how to find where a disk is mounted on this computer.
package idx

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/sim186/disk-sidecar/internal/assets"
	_ "modernc.org/sqlite" // pure Go SQLite, with FTS5: no C compiler, builds for every system
)

// SchemaVersion changes only when the layout can no longer be handled by older programs.
const SchemaVersion = 1

const ddl = `
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT);
CREATE TABLE IF NOT EXISTS sources (
    id INTEGER PRIMARY KEY, label TEXT UNIQUE NOT NULL, root TEXT, host TEXT, scanned_at INTEGER);
CREATE TABLE IF NOT EXISTS files (
    id INTEGER PRIMARY KEY,
    source_id INTEGER NOT NULL REFERENCES sources(id),
    path TEXT NOT NULL,
    name TEXT NOT NULL,
    ext TEXT,
    size INTEGER NOT NULL,
    mtime INTEGER NOT NULL,
    kind TEXT NOT NULL,
    top TEXT,
    sha256 TEXT,
    seen INTEGER NOT NULL DEFAULT 0,
    UNIQUE (source_id, path));
CREATE INDEX IF NOT EXISTS files_size ON files(size);
CREATE INDEX IF NOT EXISTS files_hash ON files(sha256);
CREATE INDEX IF NOT EXISTS files_kind ON files(kind);
CREATE INDEX IF NOT EXISTS files_top ON files(source_id, top);
CREATE TABLE IF NOT EXISTS marks (
    source_id INTEGER NOT NULL, path TEXT NOT NULL,
    mark TEXT NOT NULL CHECK (mark IN ('keep','delete','review')), note TEXT,
    PRIMARY KEY (source_id, path));
CREATE VIRTUAL TABLE IF NOT EXISTS file_text USING fts5(
    path, body, tokenize='unicode61 remove_diacritics 2');
CREATE TABLE IF NOT EXISTS quarantine_moves (
    id INTEGER PRIMARY KEY, batch TEXT, source_id INTEGER, path TEXT, qpath TEXT,
    is_dir INTEGER, nfiles INTEGER, size INTEGER, moved_at INTEGER, state TEXT DEFAULT 'moved');
CREATE TABLE IF NOT EXISTS quarantine_files (
    move_id INTEGER, path TEXT, name TEXT, ext TEXT, size INTEGER, mtime INTEGER,
    kind TEXT, top TEXT, sha256 TEXT);
CREATE INDEX IF NOT EXISTS quarantine_files_move ON quarantine_files(move_id);
CREATE TABLE IF NOT EXISTS scan_log (
    time INTEGER, source_id INTEGER, kind TEXT, new INTEGER, changed INTEGER,
    removed INTEGER, errors INTEGER, secs REAL);
`

// CombinedDDL is the layout of the temporary index the page receives: no quarantine, log or text
// tables, plus two columns on sources.
const CombinedDDL = `
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT);
CREATE TABLE sources (id INTEGER PRIMARY KEY, label TEXT UNIQUE NOT NULL, root TEXT, host TEXT, scanned_at INTEGER,
    kind TEXT, disk_id TEXT);
CREATE TABLE files (
    id INTEGER PRIMARY KEY, source_id INTEGER NOT NULL, path TEXT NOT NULL, name TEXT NOT NULL, ext TEXT,
    size INTEGER NOT NULL, mtime INTEGER NOT NULL, kind TEXT NOT NULL, top TEXT, sha256 TEXT,
    seen INTEGER NOT NULL DEFAULT 0, UNIQUE (source_id, path));
CREATE INDEX files_size ON files(size);
CREATE INDEX files_hash ON files(sha256);
CREATE INDEX files_kind ON files(kind);
CREATE INDEX files_top ON files(source_id, top);
CREATE TABLE marks (source_id INTEGER NOT NULL, path TEXT NOT NULL, mark TEXT NOT NULL, note TEXT, PRIMARY KEY (source_id, path));
`

// Kinds maps a lower-case extension to the kind of file shown in the app.
var Kinds = func() map[string]string {
	groups := map[string]string{
		"photo":     "jpg jpeg png heic gif tif tiff dng nef arw cr2 cr3 raf orf rw2 webp bmp psd xmp",
		"video":     "mkv mp4 mov avi m4v wmv mpg mpeg mts m2ts flv webm",
		"audio":     "mp3 flac wav m4a aac ogg aiff",
		"document":  "pdf doc docx xls xlsx ppt pptx odt ods txt md rtf csv epub pages numbers key",
		"code":      "py js ts tsx jsx c h cpp hpp cs java kt swift go rs ino html css json yml yaml xml sh sql php rb class pyc",
		"installer": "exe msi dmg pkg iso deb apk inf dll sys",
		"archive":   "zip rar 7z tar gz tgz bz2 xz",
		"lightroom": "lrcat lrdata lrprev lrfprev lrtemplate",
	}
	m := map[string]string{}
	for k, exts := range groups {
		for _, e := range strings.Fields(exts) {
			m[e] = k
		}
	}
	return m
}()

// KindOf returns the kind for an extension without the dot.
func KindOf(ext string) string {
	if k, ok := Kinds[ext]; ok {
		return k
	}
	return "other"
}

// Folders and files that are never indexed.
var SkipDirs = map[string]bool{"$RECYCLE.BIN": true, "System Volume Information": true, ".Spotlight-V100": true,
	".Trashes": true, ".fseventsd": true, ".TemporaryItems": true, ".sidecar": true}
var SkipFiles = map[string]bool{".DS_Store": true, "Thumbs.db": true, "desktop.ini": true}

// Skip reports whether a file name is never indexed (system noise and macOS ._ files).
func SkipFile(name string) bool { return SkipFiles[name] || strings.HasPrefix(name, "._") }

// FileURI turns a path into a SQLite file: URI, which also makes ATTACH accept URIs.
func FileURI(path, params string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	p := filepath.ToSlash(abs)
	if runtime.GOOS == "windows" && !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	u := url.URL{Scheme: "file", Path: p}
	return u.String() + "?" + params
}

// Open opens (and creates) an index for writing and makes sure the layout is there.
func Open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", FileURI(path, "_pragma=busy_timeout(30000)"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(ddl); err != nil {
		db.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := CheckSchema(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// OpenRO opens an index read-only; it never creates or changes anything.
func OpenRO(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", FileURI(path, "mode=ro&_pragma=busy_timeout(30000)"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// CheckSchema refuses to touch an index written by a newer Disk Sidecar than this one.
func CheckSchema(db *sql.DB) error {
	var v string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&v); err != nil {
		return nil // no meta yet: a new index
	}
	if n, _ := strconv.Atoi(v); n > SchemaVersion {
		return fmt.Errorf("this index uses schema %d, newer than this program supports (%d); update Disk Sidecar (this is version %s)",
			n, SchemaVersion, assets.Version)
	}
	return nil
}

// InitMeta records the layout and program versions and gives a new disk its permanent ID.
func InitMeta(db *sql.DB) error {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	stmts := []struct {
		q    string
		args []any
	}{
		{`INSERT OR REPLACE INTO meta VALUES ('schema_version', ?)`, []any{strconv.Itoa(SchemaVersion)}},
		{`INSERT OR REPLACE INTO meta VALUES ('sidecar_version', ?)`, []any{assets.Version}},
		{`INSERT OR IGNORE INTO meta VALUES ('disk_id', ?)`, []any{hex.EncodeToString(b[:])}},
	}
	for _, s := range stmts {
		if _, err := db.Exec(s.q, s.args...); err != nil {
			return err
		}
	}
	return nil
}

// DiskRootOf returns the disk an index lives on: the folder that holds its ".sidecar" folder,
// or "" if the index is not inside one.
func DiskRootOf(dbPath string) string {
	dir, err := filepath.Abs(filepath.Dir(dbPath))
	if err != nil || filepath.Base(dir) != ".sidecar" {
		return ""
	}
	return filepath.Dir(dir)
}

// ResolveRoot answers "where is this source mounted on THIS computer?". The index remembers the
// path used at scan time (/Volumes/MyDisk); on another computer the same disk has another path
// (E:\). If the stored root is missing, check that a few indexed files exist with the same size
// under the disk that holds this index, and if so use that.
func ResolveRoot(db *sql.DB, sid int64, root, diskRoot string) string {
	if st, err := os.Stat(root); err == nil && st.IsDir() {
		return root
	}
	if diskRoot == "" {
		return root
	}
	rows, err := db.Query(`SELECT path, size FROM files WHERE source_id=? ORDER BY random() LIMIT 8`, sid)
	if err != nil {
		return root
	}
	defer rows.Close()
	n, ok := 0, 0
	for rows.Next() {
		var p string
		var size int64
		if rows.Scan(&p, &size) != nil {
			continue
		}
		n++
		if st, err := os.Stat(filepath.Join(diskRoot, filepath.FromSlash(p))); err == nil && st.Mode().IsRegular() && st.Size() == size {
			ok++
		}
	}
	if n > 0 && ok >= max(1, n-1) {
		return diskRoot
	}
	return root
}
