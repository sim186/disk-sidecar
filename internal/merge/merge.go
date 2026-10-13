// Package merge imports the sources of another index into this one as full sources.
package merge

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sim186/disk-sidecar/internal/idx"
)

// Options for Run.
type Options struct {
	Main, Other string
	Labels      []string          // only these source labels (empty = all)
	Rename      map[string]string // label in other -> label in main
	DryRun      bool
	NoBackup    bool
	Log         io.Writer
}

// Run merges. A source with the same label in the main index is replaced by the incoming one.
func Run(o Options) error {
	logf := func(format string, a ...any) {
		if o.Log != nil {
			fmt.Fprintf(o.Log, format+"\n", a...)
		}
	}
	if _, err := os.Stat(o.Other); err != nil {
		return fmt.Errorf("not found: %s", o.Other)
	}
	a1, _ := filepath.Abs(o.Main)
	a2, _ := filepath.Abs(o.Other)
	if a1 == a2 {
		return fmt.Errorf("main and other index are the same file")
	}
	ro, err := idx.OpenRO(o.Other)
	if err != nil {
		return err
	}
	type src struct {
		id                int64
		label, root, host string
		scannedAt         int64
	}
	var incoming []src
	rows, err := ro.Query(`SELECT id, label, COALESCE(root,''), COALESCE(host,''), COALESCE(scanned_at,0) FROM sources ORDER BY id`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var s src
		if rows.Scan(&s.id, &s.label, &s.root, &s.host, &s.scannedAt) == nil {
			if len(o.Labels) == 0 || contains(o.Labels, s.label) {
				incoming = append(incoming, s)
			}
		}
	}
	rows.Close()
	ro.Close()
	for _, l := range o.Labels {
		found := false
		for _, s := range incoming {
			found = found || s.label == l
		}
		if !found {
			return fmt.Errorf("no such source in %s: %s", o.Other, l)
		}
	}
	if len(incoming) == 0 {
		return fmt.Errorf("nothing to import")
	}
	if !o.DryRun && !o.NoBackup {
		if _, err := os.Stat(o.Main); err == nil {
			if err := copyFile(o.Main, o.Main+".bak"); err != nil {
				return err
			}
		}
	}
	db, err := idx.Open(o.Main)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec(`ATTACH DATABASE ? AS o`, idx.FileURI(o.Other, "mode=ro")); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	for _, s := range incoming {
		label := s.label
		if n, ok := o.Rename[s.label]; ok {
			label = n
		}
		var nIn int
		var sIn int64
		tx.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size),0) FROM o.files WHERE source_id=?`, s.id).Scan(&nIn, &sIn)
		sid, replaced, err := idx.ReplaceSource(tx, label, s.root, s.host, s.scannedAt)
		if err != nil {
			tx.Rollback()
			return err
		}
		action := "add"
		if replaced {
			action = "replace"
		}
		logf("%-8s '%s' <- '%s': %d files, %.1f GB", action, label, s.label, nIn, float64(sIn)/1e9)
		n, err := idx.CopySource(tx, "o", s.id, sid, true)
		if err != nil {
			tx.Rollback()
			return err
		}
		logf("         text of %d files imported", n)
	}
	if o.DryRun {
		tx.Rollback()
		logf("dry run: nothing written")
		return nil
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	var total int
	var size int64
	db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size),0) FROM files`).Scan(&total, &size)
	logf("done. main index now: %d files, %.1f GB. Backup: %s.bak", total, float64(size)/1e9, o.Main)
	return nil
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

var _ = strings.TrimSpace
