package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/sim186/disk-sidecar/internal/catalog"
	"github.com/sim186/disk-sidecar/internal/farewell"
	"github.com/sim186/disk-sidecar/internal/idx"
)

type copyReq struct {
	Items []farewell.Item `json:"items"`
	To    farewell.Item   `json:"to"` // the folder to copy into: a disk, and a path on it ("" is the root of the disk)
}

const partSuffix = ".sidecar-part"

// doCopy copies files and folders from any disk to a folder on any disk (the same one too). The
// originals are never touched. Every file is written under a temporary name, read back and
// compared with what was read from the source, and only then given its final name; the copies
// are added to the index of their disk so they show up at once.
func (s *Server) doCopy(req copyReq, job *Job) (any, error) {
	if len(req.Items) == 0 {
		return nil, refuse("nothing to copy")
	}
	sel, err := s.box(job).Check(req.Items, "copied")
	if err != nil {
		return nil, err
	}
	defer sel.Close()

	srcs := s.catalog().Sources()
	var dst *catalog.Source
	for _, x := range srcs {
		if x.Label == req.To.Source {
			dst = x
		}
	}
	if dst == nil {
		return nil, refuse("unknown disk: %s", req.To.Source)
	}
	if dst.Kind != "disk" {
		return nil, refuse("%s is %s: nothing can be copied into it", dst.Label, dst.ReadOnlyWhat())
	}
	if !catalog.DirExists(dst.Root) {
		return nil, refuse("%s: the disk is not reachable from this computer (%s)", dst.Label, dst.Root)
	}
	destRel := strings.Trim(strings.ReplaceAll(req.To.Path, "\\", "/"), "/")
	if destRel != "" {
		if destRel, err = catalog.CleanRel(destRel); err != nil {
			return nil, err
		}
	}
	destDir := filepath.Join(dst.Root, filepath.FromSlash(destRel))
	anc, below := catalog.ExistingAncestor(destDir)
	if st, err := os.Stat(anc); err != nil || !st.IsDir() {
		return nil, refuse("%s: not a folder", destRel)
	}
	if catalog.Realpath(anc) != catalog.Realpath(dst.Root) && !catalog.Inside(dst.Root, anc) {
		return nil, refuse("%s: outside the disk", destRel)
	}
	if err := s.guardPath(filepath.Join(catalog.Realpath(anc), below)); err != nil {
		return nil, refuse("%s: %v", destRel, err)
	}
	if lst, err := os.Lstat(destDir); err == nil && lst.Mode()&os.ModeSymlink != 0 {
		return nil, refuse("%s: is a symbolic link", destRel)
	}

	// everything is checked before the first byte is written
	type job1 struct {
		m      *farewell.Pick
		target string // final folder or file
		base   string
	}
	var jobs []job1
	seen := map[string]bool{}
	var total int64
	for _, m := range sel.Picks {
		base := path.Base(m.Rel)
		target := filepath.Join(destDir, base)
		if seen[strings.ToLower(base)] {
			return nil, refuse("two of the items are both called %q: copy them one at a time", base)
		}
		seen[strings.ToLower(base)] = true
		if _, err := os.Lstat(target); err == nil {
			return nil, refuse("%s: already exists in the destination; not overwriting", path.Join(destRel, base))
		}
		if m.IsDir {
			rm, rt := catalog.Realpath(m.Full), filepath.Join(catalog.Realpath(anc), below)
			if rt == rm || strings.HasPrefix(rt+string(filepath.Separator), rm+string(filepath.Separator)) {
				return nil, refuse("%s: cannot copy a folder into itself", m.Rel)
			}
		}
		for _, r := range m.Files {
			total += r.Size
		}
		jobs = append(jobs, job1{m, target, base})
	}
	if job != nil {
		job.Update("Copying…", 0, total)
	}

	ddb := sel.Index(dst.DB)
	if ddb == nil {
		if ddb, err = idx.Open(dst.DB); err != nil {
			return nil, err
		}
		defer ddb.Close()
	}
	catalog.CopyFile(dst.DB, dst.DB+".bak")
	if below != "" {
		if err := os.MkdirAll(destDir, 0o755); err != nil {
			return nil, err
		}
	}

	var nfiles, nitems int
	var nbytes int64
	for _, j := range jobs {
		m := j.m
		type made struct {
			row  idx.File
			rel  string
			sha  string
			body sql.NullString
		}
		var list []made
		fail := func(err error) (any, error) {
			os.RemoveAll(j.target) // the target did not exist before: all of it is ours
			return nil, refuse("%s: copy failed (%v). Nothing of it was kept. Copied so far: %d items.", m.Rel, err, nitems)
		}
		for _, r := range m.Files {
			sub := "" // where this file goes under the target folder
			if m.IsDir {
				sub = strings.TrimPrefix(r.Path, m.Rel+"/")
			}
			from := filepath.Join(m.Source.Root, filepath.FromSlash(r.Path))
			to := j.target
			if m.IsDir {
				to = filepath.Join(j.target, filepath.FromSlash(sub))
			}
			if job != nil {
				job.Phase("Copying " + path.Base(r.Path))
			}
			sum, err := copyVerified(from, to, r, job)
			if err != nil {
				return fail(err)
			}
			if r.SHA.Valid && r.SHA.String != sum {
				os.Remove(to)
				return fail(fmt.Errorf("%s no longer matches the index: it was changed since the last scan", r.Path))
			}
			rel := path.Join(destRel, j.base)
			if m.IsDir {
				rel = path.Join(rel, sub)
			}
			list = append(list, made{r, rel, sum, idx.Text(m.DB, r.ID)})
		}
		tx, err := ddb.Begin()
		if err != nil {
			return nil, err
		}
		fw, err := idx.NewFileWriter(tx, dst.SID)
		if err != nil {
			tx.Rollback()
			return nil, err
		}
		for _, c := range list {
			id, err := fw.Put(c.rel, c.row.Size, c.row.Mtime, sql.NullString{String: c.sha, Valid: true})
			if err == nil && c.body.Valid {
				err = fw.PutText(id, c.rel, c.body.String)
			}
			if err != nil {
				fw.Close()
				tx.Rollback()
				return nil, fmt.Errorf("the files were copied but the index was not updated (%v); use Check now", err)
			}
		}
		fw.Close()
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		var ibytes int64
		for _, c := range list { // the source learns its hash too: the copy is now a proven duplicate
			if !c.row.SHA.Valid {
				idx.SetHash(m.DB, c.row.ID, c.sha)
			}
			ibytes += c.row.Size
		}
		nbytes += ibytes
		nfiles += len(list)
		nitems++
		m.Source.Disk.Log("copy-from", m.Source.Raw, m.Rel, len(list), 0)
		dst.Disk.Log("copy-to", dst.Label, path.Join(destRel, j.base), len(list), ibytes)
	}
	return map[string]any{"copied": nitems, "files": nfiles, "bytes": nbytes}, nil
}

// copyVerified copies from to to (which must not exist) through a temporary file, checks what
// was written by reading it back, restores the modification time and returns the SHA-256.
func copyVerified(from, to string, r idx.File, job *Job) (string, error) {
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
	tmp := to + partSuffix
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
			if job != nil {
				job.Advance(int64(k))
			}
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
