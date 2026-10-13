// Package transfer copies files into a folder on a disk and proves every copy: each file is
// written under a temporary name, its SHA-256 is checked against what was read, and only then does
// it get its name and join the index of its disk. Nothing is ever overwritten, and the originals
// are never touched.
//
// Where the bytes come from is an Origin: a disk on this computer (FromDisks) or an Android phone
// (FromPhone).
package transfer

import (
	"database/sql"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/sim186/disk-sidecar/internal/catalog"
	"github.com/sim186/disk-sidecar/internal/farewell"
	"github.com/sim186/disk-sidecar/internal/idx"
	"github.com/sim186/disk-sidecar/internal/refusal"
)

// PartSuffix marks a file that is still being written.
const PartSuffix = ".sidecar-part"

// Item is one file or folder to copy, with the files it holds.
type Item struct {
	Source string // the label of where it comes from
	Rel    string // its path there
	IsDir  bool
	Local  string // where it is on this computer; "" if it is not here (a phone)
	Files  []idx.File
	pick   *farewell.Pick
}

// Origin is where the bytes come from.
type Origin interface {
	// Fetch writes file f of it to `to`, which must not exist, through a temporary file; it proves
	// what arrived and returns its SHA-256.
	Fetch(it Item, f idx.File, to string, p farewell.Progress) (string, error)
	// Text is the searchable text the origin's index keeps for f.
	Text(it Item, f idx.File) sql.NullString
	// Copied is told that the files of it now have a proven copy, and their hashes.
	Copied(it Item, sums map[int64]string)
}

// Copier copies into the disks of Catalog.
type Copier struct {
	Catalog *catalog.Catalog
	// Guard says whether a path on this computer may be changed; nil allows everything inside a disk.
	Guard    func(full string) error
	Progress farewell.Progress // nil: silent
}

// Copied is what Copy did.
type Copied struct {
	Copied int   `json:"copied"`
	Files  int   `json:"files"`
	Bytes  int64 `json:"bytes"`
}

type nop struct{}

func (nop) Update(string, int64, int64) {}
func (nop) Phase(string)                {}
func (nop) SetDone(int64)               {}
func (nop) Advance(int64)               {}

// Copy copies items from origin into the folder to (a disk, and a path on it; "" is its root),
// which is made if needed. Everything is checked before the first byte is written. Items are
// copied one at a time: if a file of one fails, nothing of that item is kept.
func (c Copier) Copy(items []Item, from Origin, to farewell.Item) (Copied, error) {
	var res Copied
	p := c.Progress
	if p == nil {
		p = nop{}
	}
	if len(items) == 0 {
		return res, refusal.New("nothing to copy")
	}
	dst, err := c.Catalog.Writable(to.Source, "nothing can be copied into it")
	if err != nil {
		return res, err
	}
	if !catalog.DirExists(dst.Root) {
		return res, refusal.New("%s: the disk is not reachable from this computer (%s)", dst.Label, dst.Root)
	}
	destRel := strings.Trim(strings.ReplaceAll(to.Path, "\\", "/"), "/")
	if destRel != "" {
		if destRel, err = catalog.CleanRel(destRel); err != nil {
			return res, err
		}
	}
	destDir := filepath.Join(dst.Root, filepath.FromSlash(destRel))
	anc, below := catalog.ExistingAncestor(destDir)
	if st, err := os.Stat(anc); err != nil || !st.IsDir() {
		return res, refusal.New("%s: not a folder", destRel)
	}
	if catalog.Realpath(anc) != catalog.Realpath(dst.Root) && !catalog.Inside(dst.Root, anc) {
		return res, refusal.New("%s: outside the disk", destRel)
	}
	realDest := filepath.Join(catalog.Realpath(anc), below)
	if c.Guard != nil {
		if err := c.Guard(realDest); err != nil {
			return res, refusal.New("%s: %v", destRel, err)
		}
	}
	if lst, err := os.Lstat(destDir); err == nil && lst.Mode()&os.ModeSymlink != 0 {
		return res, refusal.New("%s: is a symbolic link", destRel)
	}

	seen := map[string]bool{}
	var total int64
	for _, it := range items {
		base := path.Base(it.Rel)
		if seen[strings.ToLower(base)] {
			return res, refusal.New("two of the items are both called %q: copy them one at a time", base)
		}
		seen[strings.ToLower(base)] = true
		if _, err := os.Lstat(filepath.Join(destDir, base)); err == nil {
			return res, refusal.New("%s: already exists in the destination; not overwriting", path.Join(destRel, base))
		}
		if it.IsDir && it.Local != "" {
			rm := catalog.Realpath(it.Local)
			if realDest == rm || strings.HasPrefix(realDest+string(filepath.Separator), rm+string(filepath.Separator)) {
				return res, refusal.New("%s: cannot copy a folder into itself", it.Rel)
			}
		}
		for _, f := range it.Files {
			total += f.Size
		}
	}
	p.Update("Copying…", 0, total)

	var ddb *sql.DB
	if o, ok := from.(interface{ index(string) *sql.DB }); ok {
		ddb = o.index(dst.DB) // SQLite: one handle per file, or the two wait on each other
	}
	if ddb == nil {
		if ddb, err = idx.Open(dst.DB); err != nil {
			return res, err
		}
		defer ddb.Close()
	}
	dst.Disk.Backup()
	if below != "" {
		if err := os.MkdirAll(destDir, 0o755); err != nil {
			return res, err
		}
	}

	for _, it := range items {
		base := path.Base(it.Rel)
		target := filepath.Join(destDir, base)
		itemRel := path.Join(destRel, base)
		fail := func(err error) (Copied, error) {
			os.RemoveAll(target) // it did not exist before: all of it is ours
			return res, refusal.New("%s: copy failed (%v). Nothing of it was kept. Copied so far: %d items.", it.Rel, err, res.Copied)
		}
		rels := make([]string, len(it.Files))
		sums := map[int64]string{}
		for i, f := range it.Files {
			rel, to := itemRel, target
			if it.IsDir {
				sub := strings.TrimPrefix(f.Path, it.Rel+"/")
				rel, to = path.Join(itemRel, sub), filepath.Join(target, filepath.FromSlash(sub))
			}
			p.Phase("Copying " + path.Base(f.Path))
			sum, err := from.Fetch(it, f, to, p)
			if err != nil {
				return fail(err)
			}
			if f.SHA.Valid && f.SHA.String != sum {
				return fail(fmt.Errorf("%s no longer matches the index: it was changed since the last scan", f.Path))
			}
			rels[i], sums[f.ID] = rel, sum
		}
		if err := record(ddb, dst.SID, it, rels, sums, from); err != nil {
			return res, fmt.Errorf("the files were copied but the index was not updated (%v); use Check now", err)
		}
		from.Copied(it, sums)
		var bytes int64
		for _, f := range it.Files {
			bytes += f.Size
		}
		dst.Disk.Log("copy-to", dst.Label, itemRel+" (from "+it.Source+")", len(it.Files), bytes)
		res.Copied++
		res.Files += len(it.Files)
		res.Bytes += bytes
	}
	return res, nil
}

// record adds the copies of it to the index of their disk, with the text the origin knows.
func record(db *sql.DB, sid int64, it Item, rels []string, sums map[int64]string, from Origin) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	fw, err := idx.NewFileWriter(tx, sid)
	if err != nil {
		tx.Rollback()
		return err
	}
	for i, f := range it.Files {
		id, err := fw.Put(rels[i], f.Size, f.Mtime, sql.NullString{String: sums[f.ID], Valid: true})
		if body := from.Text(it, f); err == nil && body.Valid {
			err = fw.PutText(id, rels[i], body.String)
		}
		if err != nil {
			fw.Close()
			tx.Rollback()
			return err
		}
	}
	fw.Close()
	return tx.Commit()
}
