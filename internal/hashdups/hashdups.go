// Package hashdups confirms duplicates: SHA-256 only for files that share their size with
// another file (on this disk, or on another index given as "also match").
package hashdups

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/sim186/disk-sidecar/internal/idx"
	"github.com/sim186/disk-sidecar/internal/scan"
)

// Options for Run.
type Options struct {
	DB        string
	MinMB     float64
	AlsoMatch string            // another index whose file sizes count as "ties"
	Roots     map[string]string // label -> folder, when a disk is mounted somewhere else
	Log       io.Writer
}

// Result of a run.
type Result struct {
	Hashed, Errors int
	Groups         int
	Recoverable    int64
	Secs           float64
}

// Run hashes what is needed. Resumable: files that already have a hash are skipped.
func Run(o Options) (Result, error) {
	var res Result
	t0 := time.Now()
	logf := func(format string, a ...any) {
		if o.Log != nil {
			fmt.Fprintf(o.Log, format+"\n", a...)
		}
	}
	db, err := idx.Open(o.DB)
	if err != nil {
		return res, err
	}
	defer db.Close()
	diskRoot := idx.DiskRootOf(o.DB)
	roots := map[int64]string{}
	srcRows, err := db.Query(`SELECT id, label, root FROM sources`)
	if err != nil {
		return res, err
	}
	type src struct {
		id          int64
		label, root string
	}
	var srcs []src
	for srcRows.Next() {
		var s src
		if srcRows.Scan(&s.id, &s.label, &s.root) == nil {
			srcs = append(srcs, s)
		}
	}
	srcRows.Close()
	for _, s := range srcs {
		r := s.root
		if over, ok := o.Roots[s.label]; ok {
			r = over
		} else {
			r = idx.ResolveRoot(db, s.id, s.root, diskRoot)
		}
		roots[s.id] = r
		if st, err := os.Stat(r); err != nil || !st.IsDir() {
			logf("warning: root not found for source %q: %s (its files are skipped)", s.label, r)
		}
	}
	minSize := int64(o.MinMB * 1024 * 1024)
	ties := `SELECT size FROM files WHERE size >= ? GROUP BY size HAVING COUNT(*) > 1`
	args := []any{minSize, minSize}
	if o.AlsoMatch != "" {
		abs, _ := filepath.Abs(o.AlsoMatch)
		if _, err := os.Stat(abs); err != nil {
			return res, fmt.Errorf("not found: %s", o.AlsoMatch)
		}
		if _, err := db.Exec(`ATTACH DATABASE ? AS other`, idx.FileURI(abs, "mode=ro")); err != nil {
			return res, err
		}
		ties += ` UNION SELECT size FROM other.files WHERE size >= ?`
		args = append(args, minSize)
	}
	type row struct {
		id, sid, size int64
		path          string
	}
	q, err := db.Query(`SELECT id, source_id, path, size FROM files WHERE sha256 IS NULL AND size >= ? AND size IN (`+ties+`) ORDER BY size DESC`,
		append([]any{minSize}, args[1:]...)...)
	if err != nil {
		return res, err
	}
	var todo []row
	var total int64
	for q.Next() {
		var r row
		if q.Scan(&r.id, &r.sid, &r.path, &r.size) != nil {
			continue
		}
		if st, err := os.Stat(roots[r.sid]); err != nil || !st.IsDir() {
			continue
		}
		todo = append(todo, r)
		total += r.size
	}
	q.Close()
	logf("%d files to hash, %.1f GB", len(todo), float64(total)/1e9)

	var nbytes int64
	tx, err := db.Begin()
	if err != nil {
		return res, err
	}
	for i, r := range todo {
		sum, err := scan.SHA256(filepath.Join(roots[r.sid], filepath.FromSlash(r.path)))
		if err != nil {
			res.Errors++
			logf("error %s: %v", r.path, err)
			continue
		}
		if _, err := tx.Exec(`UPDATE files SET sha256=? WHERE id=?`, sum, r.id); err != nil {
			tx.Rollback()
			return res, err
		}
		res.Hashed++
		nbytes += r.size
		if (i+1)%100 == 0 {
			if err := tx.Commit(); err != nil {
				return res, err
			}
			if tx, err = db.Begin(); err != nil {
				return res, err
			}
			el := time.Since(t0).Seconds()
			logf("  %d/%d files, %.1f/%.1f GB, %.0f MB/s", i+1, len(todo), float64(nbytes)/1e9, float64(total)/1e9, float64(nbytes)/1e6/el)
		}
	}
	if err := tx.Commit(); err != nil {
		return res, err
	}
	var rec sql.NullInt64
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(w),0) FROM (SELECT size*(COUNT(*)-1) w FROM files WHERE sha256 IS NOT NULL GROUP BY sha256 HAVING COUNT(*)>1)`).
		Scan(&res.Groups, &rec); err != nil {
		return res, err
	}
	res.Recoverable = rec.Int64
	res.Secs = time.Since(t0).Seconds()
	return res, nil
}

// Summary is the one-line result of a run.
func (r Result) Summary() string {
	return fmt.Sprintf("hashed %d files (%d errors) in %.0fs | confirmed duplicate groups: %d, recoverable: %.1f GB",
		r.Hashed, r.Errors, r.Secs, r.Groups, float64(r.Recoverable)/1e9)
}
