package transfer

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/sim186/disk-sidecar/internal/farewell"
	"github.com/sim186/disk-sidecar/internal/idx"
)

// FromDisks is the origin of items checked on disks (farewell.Box.Check): files on this computer,
// as the index says they are.
func FromDisks(sel *farewell.Selection) ([]Item, Origin) {
	items := make([]Item, len(sel.Picks))
	for i, p := range sel.Picks {
		items[i] = Item{Source: p.Source.Label, Rel: p.Rel, IsDir: p.IsDir, Local: p.Full, Files: p.Files, pick: p}
	}
	return items, disks{sel}
}

type disks struct{ sel *farewell.Selection }

func (d disks) index(path string) *sql.DB { return d.sel.Index(path) }

func (d disks) Fetch(it Item, f idx.File, to string, p farewell.Progress) (string, error) {
	return copyVerified(filepath.Join(it.pick.Source.Root, filepath.FromSlash(f.Path)), to, f, p)
}

func (d disks) Text(it Item, f idx.File) sql.NullString { return idx.Text(it.pick.DB, f.ID) }

// Copied teaches the originals their hash: each is now a proven duplicate.
func (d disks) Copied(it Item, sums map[int64]string) {
	for _, f := range it.Files {
		if !f.SHA.Valid {
			idx.SetHash(it.pick.DB, f.ID, sums[f.ID])
		}
	}
	it.pick.Source.Disk.Log("copy-from", it.pick.Source.Raw, it.Rel, len(it.Files), 0)
}

// copyVerified copies from to to (which must not exist) through a temporary file, checks what
// was written by reading it back, restores the modification time and returns the SHA-256.
func copyVerified(from, to string, r idx.File, p farewell.Progress) (string, error) {
	in, err := os.Open(from)
	if err != nil {
		return "", err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return "", err
	}
	if st.Size() != r.Size || st.ModTime().Unix() != r.Mtime {
		return "", fmt.Errorf("%s changed since the last scan", r.Path)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return "", err
	}
	if _, err := os.Lstat(to); err == nil {
		return "", fmt.Errorf("%s already exists", to)
	}
	tmp := to + PartSuffix
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		if !ok {
			out.Close()
			os.Remove(tmp)
		}
	}()
	h := sha256.New()
	buf := make([]byte, 1<<20)
	var n int64
	for {
		k, rerr := in.Read(buf)
		if k > 0 {
			h.Write(buf[:k])
			if _, werr := out.Write(buf[:k]); werr != nil {
				return "", werr
			}
			n += int64(k)
			p.Advance(int64(k))
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", rerr
		}
	}
	if n != r.Size {
		return "", fmt.Errorf("%s changed while it was being copied", r.Path)
	}
	if err := out.Sync(); err != nil {
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	back, err := os.Open(tmp) // read it back from the destination
	if err != nil {
		return "", err
	}
	h2 := sha256.New()
	_, err = io.CopyBuffer(h2, back, buf)
	back.Close()
	if err != nil {
		return "", err
	}
	if hex.EncodeToString(h2.Sum(nil)) != sum {
		return "", fmt.Errorf("%s was not written correctly (the copy does not match)", r.Path)
	}
	mt := time.Unix(r.Mtime, 0)
	os.Chtimes(tmp, mt, mt)
	os.Chmod(tmp, st.Mode().Perm()|0o600)
	if _, err := os.Lstat(to); err == nil { // os.Rename would silently replace it
		return "", fmt.Errorf("%s already exists", to)
	}
	if err := os.Rename(tmp, to); err != nil {
		return "", err
	}
	ok = true
	return sum, nil
}
