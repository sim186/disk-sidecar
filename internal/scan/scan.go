// Package scan walks a disk (read only) and records every file in the index. It is
// incremental: files whose size and date did not change are skipped.
package scan

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/sim186/disk-sidecar/internal/idx"
)

const maxTextBytes = 256 * 1024

// Folders whose contents are indexed by name but not by text: libraries and build output are
// huge and nobody searches them.
var noTextDirs = map[string]bool{"node_modules": true, ".git": true, ".out": true, "build": true, "vcpkg_installed": true,
	"target": true, "venv": true, ".venv": true, "__pycache__": true, "Pods": true, "site-packages": true, ".gradle": true}

// noTextKey names the current list of noTextDirs, to know whether old text in them was cleaned.
var noTextKey = func() string {
	var ds []string
	for d := range noTextDirs {
		ds = append(ds, d)
	}
	sort.Strings(ds)
	return strings.Join(ds, "/")
}()

func textAllowed(rel string) bool {
	parts := strings.Split(rel, "/")
	for _, p := range parts[:len(parts)-1] {
		if noTextDirs[p] {
			return false
		}
	}
	return true
}

var textExts = map[string]bool{}

func init() {
	for _, e := range strings.Fields("txt md csv py js ts tsx jsx c h cpp hpp cs java kt swift go rs ino html css json yml yaml xml sh sql php rb") {
		textExts[e] = true
	}
}

// Options for Run.
type Options struct {
	Root, DB, Label, Host string
	HashMin               int64 // also SHA-256 files of at least this size; <0 = never
	Text                  bool  // extract text for the full-text search
	Progress              func(seen int)
	Log                   io.Writer // nil = silent
	// Entries, when set, lists the files instead of walking Root: for a source that is not a folder
	// on this computer (a phone read through adb). Nothing is hashed or read for text then.
	Entries func(yield func(Entry) error) error
	// WriteLock, when set, is held around each write transaction, so another writer in the same
	// program (a click on Keep or Let go) gets its turn between two of them instead of starving:
	// SQLite's own busy wait keeps missing the moment the lock is free.
	WriteLock sync.Locker
}

// Entry is one file reported by Options.Entries; Path is relative, with forward slashes.
type Entry struct {
	Path        string
	Size, Mtime int64
}

// Stats is what a scan found.
type Stats struct {
	Total, New, Changed, Same, Removed, Errors int
	Size                                       int64
	Secs                                       float64
}

// ReadText returns the searchable text of a file, or "".
func ReadText(path, ext string) string {
	switch {
	case textExts[ext]:
		f, err := os.Open(path)
		if err != nil {
			return ""
		}
		defer f.Close()
		b, _ := io.ReadAll(io.LimitReader(f, maxTextBytes))
		return strings.ToValidUTF8(string(b), "�")
	case ext == "pdf":
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "pdftotext", "-q", "-l", "30", path, "-").Output()
		if err != nil {
			return ""
		}
		if len(out) > maxTextBytes {
			out = out[:maxTextBytes]
		}
		if !utf8.Valid(out) {
			return strings.ToValidUTF8(string(out), "�")
		}
		return string(out)
	}
	return ""
}

// SHA256 of a file.
func SHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, f, make([]byte, 4<<20)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// homeSkipDirs are left out at any depth when scanning the user's home folder: libraries, caches
// and environments that hold many small files nobody tidies by hand.
var homeSkipDirs = map[string]bool{"node_modules": true, "__pycache__": true, "venv": true, "site-packages": true,
	"Pods": true, "vcpkg_installed": true}

// Run scans o.Root into the index o.DB as source o.Label. It runs at background priority, so a
// scan of the disk the system runs on does not slow down the rest of the computer.
func Run(o Options) (Stats, error) {
	type result struct {
		st  Stats
		err error
	}
	c := make(chan result, 1)
	go func() {
		runtime.LockOSThread() // never unlocked: the thread ends with this goroutine, and its lowered priority with it
		background()
		st, err := run(o)
		c <- result{st, err}
	}()
	r := <-c
	return r.st, r.err
}

func run(o Options) (Stats, error) {
	t0 := time.Now()
	var st Stats
	root := o.Root
	if o.Entries == nil {
		var err error
		if root, err = filepath.Abs(o.Root); err != nil {
			return st, err
		}
		if info, err := os.Stat(root); err != nil || !info.IsDir() {
			return st, fmt.Errorf("not a folder: %s", o.Root)
		}
	}
	db, err := idx.Open(o.DB)
	if err != nil {
		return st, err
	}
	defer db.Close()
	if err := idx.InitMeta(db); err != nil {
		return st, err
	}
	sid, err := idx.EnsureSource(db, o.Label, root, o.Host)
	if err != nil {
		return st, err
	}

	homeMode := o.Entries == nil && IsHome(root) // the user's home folder: leave out the folders where programs keep their data
	type fsig struct{ size, mtime int64 }
	known := map[string]fsig{}
	rows, err := db.Query(`SELECT path, size, mtime FROM files WHERE source_id=?`, sid)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var p string
		var s fsig
		if err := rows.Scan(&p, &s.size, &s.mtime); err == nil {
			known[p] = s
		}
	}
	rows.Close()
	seen := make(map[string]struct{}, len(known))

	tx, err := db.Begin()
	if err != nil {
		return st, err
	}
	files, err := idx.NewFileWriter(tx, sid)
	if err != nil {
		return st, err
	}
	count := 0
	var dirtySince time.Time // first write of the open transaction
	held := false
	hold := func() {
		if o.WriteLock != nil && !held {
			o.WriteLock.Lock()
			held = true
		}
	}
	release := func() {
		if held {
			o.WriteLock.Unlock()
			held = false
		}
	}
	defer release()
	logf := func(format string, a ...any) {
		if o.Log != nil {
			fmt.Fprintf(o.Log, format+"\n", a...)
		}
	}

	// process records one file; local is where it can be read on this computer ("" = it cannot).
	process := func(rel string, size, mtime int64, local string) error {
		seen[rel] = struct{}{}
		count++
		if o.Progress != nil && count%500 == 0 {
			o.Progress(count)
		}
		// commit every 5000 files, and also once the write lock has been held for a moment: on a slow
		// disk 5000 files can take many seconds to walk, and a click on Keep or Let go waits for the lock
		if count%5000 == 0 || (!dirtySince.IsZero() && time.Since(dirtySince) > 200*time.Millisecond) {
			dirtySince = time.Time{}
			files.Close()
			err := tx.Commit()
			release()
			if err != nil {
				return err
			}
			if tx, err = db.Begin(); err != nil {
				return err
			}
			if files, err = idx.NewFileWriter(tx, sid); err != nil {
				return err
			}
			logf("  %d files (%.0fs)", count, time.Since(t0).Seconds())
		}
		if k, ok := known[rel]; ok && k.size == size && k.mtime == mtime {
			st.Same++
			return nil
		}
		_, ext, _ := idx.NameParts(rel)
		var sum sql.NullString
		if local != "" && o.HashMin >= 0 && size >= o.HashMin {
			if h, err := SHA256(local); err == nil {
				sum = sql.NullString{String: h, Valid: true}
			} else {
				st.Errors++
				logf("hash error %s: %v", rel, err)
			}
		}
		if dirtySince.IsZero() {
			hold()
			dirtySince = time.Now()
		}
		id, err := files.Put(rel, size, mtime, sum)
		if err != nil {
			return err
		}
		if _, existed := known[rel]; existed {
			st.Changed++
		} else {
			st.New++
		}
		if local != "" && o.Text && textAllowed(rel) {
			if body := ReadText(local, ext); body != "" {
				if err := files.PutText(id, rel, body); err != nil {
					return err
				}
			}
		}
		return nil
	}

	var walkErr error
	if o.Entries != nil {
		walkErr = o.Entries(func(e Entry) error {
			if e.Path == "" || idx.SkipFile(e.Path[strings.LastIndex(e.Path, "/")+1:]) {
				return nil
			}
			return process(e.Path, e.Size, e.Mtime, "")
		})
	} else {
		walkErr = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				st.Errors++
				logf("read error %s: %v", path, err)
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			name := d.Name()
			if d.IsDir() {
				if path != root && idx.SkipDirs[name] {
					return fs.SkipDir
				}
				if homeMode && (strings.HasPrefix(name, ".") || homeSkipDirs[name] ||
					filepath.Dir(path) == root && (name == "Library" || name == "AppData")) {
					return fs.SkipDir
				}
				return nil
			}
			if idx.SkipFile(name) {
				return nil
			}
			var info fs.FileInfo
			if d.Type()&fs.ModeSymlink != 0 { // like a normal stat: follow links to files, ignore links to folders
				info, err = os.Stat(path)
			} else {
				info, err = d.Info()
			}
			if err != nil {
				st.Errors++
				logf("stat error %s: %v", path, err)
				return nil
			}
			if info.IsDir() || !info.Mode().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return nil
			}
			local := path
			if Placeholder(info) { // only in the cloud: list it, but reading it would download it
				local = ""
			}
			return process(filepath.ToSlash(rel), info.Size(), info.ModTime().Unix(), local)
		})
	}
	files.Close()
	if walkErr != nil {
		tx.Rollback()
		return st, walkErr
	}
	hold()                 // the last transaction: files gone from the disk, and the time of this scan
	for p := range known { // gone from the disk
		if _, ok := seen[p]; ok {
			continue
		}
		if err := idx.RemovePath(tx, sid, p); err != nil {
			tx.Rollback()
			return st, err
		}
		st.Removed++
	}
	// text kept from a time when these folders were still indexed. New text never lands there, so
	// this runs once per list of folders, not at every check: each pass reads every file of the disk
	// while holding the write lock
	cleaned := fmt.Sprintf("notext_dirs:%d", sid)
	var done string
	tx.QueryRow(`SELECT value FROM meta WHERE key=?`, cleaned).Scan(&done)
	if o.Text && done != noTextKey {
		for d := range noTextDirs {
			if err := idx.DropTextUnder(tx, sid, d); err != nil {
				tx.Rollback()
				return st, err
			}
		}
		if err := idx.SetMeta(tx, cleaned, noTextKey); err != nil {
			tx.Rollback()
			return st, err
		}
	}
	if err := idx.MarkScanned(tx, sid); err != nil {
		tx.Rollback()
		return st, err
	}
	err = tx.Commit()
	release()
	if err != nil {
		return st, err
	}
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size),0) FROM files WHERE source_id=?`, sid).Scan(&st.Total, &st.Size); err != nil {
		return st, err
	}
	st.Secs = time.Since(t0).Seconds()
	return st, nil
}

// Summary is the one-line result of a scan.
func (st Stats) Summary(label string) string {
	return fmt.Sprintf("[%s] %d files, %.1f GB | new %d, changed %d, same %d, removed %d, errors %d | %.0fs", label, st.Total,
		float64(st.Size)/1e9, st.New, st.Changed, st.Same, st.Removed, st.Errors, st.Secs)
}

// IsHome tells whether root is the home folder of the user running this program, on the disk the
// system runs on. It is scanned by name only unless text is asked for, and not re-checked at start.
func IsHome(root string) bool {
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return false
	}
	a, b := root, h
	if r, err := filepath.EvalSymlinks(a); err == nil {
		a = r
	}
	if r, err := filepath.EvalSymlinks(b); err == nil {
		b = r
	}
	return filepath.Clean(a) == filepath.Clean(b)
}
