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
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sim186/disk-sidecar/internal/idx"
)

const maxTextBytes = 256 * 1024

// Folders whose contents are indexed by name but not by text: libraries and build output are
// huge and nobody searches them.
var noTextDirs = map[string]bool{"node_modules": true, ".git": true, ".out": true, "build": true, "vcpkg_installed": true,
	"target": true, "venv": true, ".venv": true, "__pycache__": true, "Pods": true, "site-packages": true, ".gradle": true}

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

// Run scans o.Root into the index o.DB as source o.Label.
func Run(o Options) (Stats, error) {
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
	if _, err := db.Exec(`INSERT INTO sources(label, root, host) VALUES (?,?,?)
		ON CONFLICT(label) DO UPDATE SET root=excluded.root, host=excluded.host`, o.Label, root, o.Host); err != nil {
		return st, err
	}
	var sid int64
	if err := db.QueryRow(`SELECT id FROM sources WHERE label=?`, o.Label).Scan(&sid); err != nil {
		return st, err
	}

	homeMode := o.Entries == nil && isHome(root) // the user's home folder: leave out the folders where programs keep their data
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
	upsert, err := tx.Prepare(`INSERT INTO files(source_id,path,name,ext,size,mtime,kind,top,sha256,seen)
		VALUES (?,?,?,?,?,?,?,?,?,1)
		ON CONFLICT(source_id,path) DO UPDATE SET size=excluded.size, mtime=excluded.mtime, kind=excluded.kind,
		top=excluded.top, sha256=excluded.sha256, seen=1 RETURNING id`)
	if err != nil {
		return st, err
	}
	count := 0
	logf := func(format string, a ...any) {
		if o.Log != nil {
			fmt.Fprintf(o.Log, format+"\n", a...)
		}
	}

	// process records one file; local is where it can be read on this computer ("" = it cannot).
	process := func(rel string, size, mtime int64, local string) error {
		name := rel
		if i := strings.LastIndex(rel, "/"); i >= 0 {
			name = rel[i+1:]
		}
		seen[rel] = struct{}{}
		count++
		if o.Progress != nil && count%500 == 0 {
			o.Progress(count)
		}
		if count%5000 == 0 {
			if err := tx.Commit(); err != nil {
				return err
			}
			if tx, err = db.Begin(); err != nil {
				return err
			}
			if upsert, err = tx.Prepare(`INSERT INTO files(source_id,path,name,ext,size,mtime,kind,top,sha256,seen)
				VALUES (?,?,?,?,?,?,?,?,?,1)
				ON CONFLICT(source_id,path) DO UPDATE SET size=excluded.size, mtime=excluded.mtime, kind=excluded.kind,
				top=excluded.top, sha256=excluded.sha256, seen=1 RETURNING id`); err != nil {
				return err
			}
			logf("  %d files (%.0fs)", count, time.Since(t0).Seconds())
		}
		if k, ok := known[rel]; ok && k.size == size && k.mtime == mtime {
			st.Same++
			return nil
		}
		ext := ""
		if i := strings.LastIndex(name, "."); i > 0 { // like Python's splitext: ".bashrc" has no extension
			ext = strings.ToLower(name[i+1:])
		}
		var sum sql.NullString
		if local != "" && o.HashMin >= 0 && size >= o.HashMin {
			if h, err := SHA256(local); err == nil {
				sum = sql.NullString{String: h, Valid: true}
			} else {
				st.Errors++
				logf("hash error %s: %v", rel, err)
			}
		}
		top := ""
		if i := strings.Index(rel, "/"); i >= 0 {
			top = rel[:i]
		}
		var id int64
		if err := upsert.QueryRow(sid, rel, name, ext, size, mtime, idx.KindOf(ext), top, sum).Scan(&id); err != nil {
			return err
		}
		if _, existed := known[rel]; existed {
			st.Changed++
		} else {
			st.New++
		}
		if local != "" && o.Text && textAllowed(rel) {
			if body := ReadText(local, ext); body != "" {
				if _, err := tx.Exec(`DELETE FROM file_text WHERE rowid=?`, id); err != nil {
					return err
				}
				if _, err := tx.Exec(`INSERT INTO file_text(rowid, path, body) VALUES (?,?,?)`, id, rel, body); err != nil {
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
				if homeMode && filepath.Dir(path) == root && (name == "Library" || name == "AppData" || strings.HasPrefix(name, ".")) {
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
			return process(filepath.ToSlash(rel), info.Size(), info.ModTime().Unix(), path)
		})
	}
	if walkErr != nil {
		tx.Rollback()
		return st, walkErr
	}
	for p := range known { // gone from the disk
		if _, ok := seen[p]; ok {
			continue
		}
		if _, err := tx.Exec(`DELETE FROM file_text WHERE rowid IN (SELECT id FROM files WHERE source_id=? AND path=?)`, sid, p); err != nil {
			tx.Rollback()
			return st, err
		}
		if _, err := tx.Exec(`DELETE FROM files WHERE source_id=? AND path=?`, sid, p); err != nil {
			tx.Rollback()
			return st, err
		}
		st.Removed++
	}
	if o.Text { // text kept from a time when these folders were still indexed
		esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
		for d := range noTextDirs {
			if _, err := tx.Exec(`DELETE FROM file_text WHERE rowid IN (SELECT id FROM files WHERE source_id=? AND (path LIKE ? ESCAPE '\' OR path LIKE ? ESCAPE '\'))`,
				sid, esc.Replace(d)+"/%", "%/"+esc.Replace(d)+"/%"); err != nil {
				tx.Rollback()
				return st, err
			}
		}
	}
	if _, err := tx.Exec(`UPDATE sources SET scanned_at=? WHERE id=?`, time.Now().Unix(), sid); err != nil {
		tx.Rollback()
		return st, err
	}
	if err := tx.Commit(); err != nil {
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

func isHome(root string) bool {
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
