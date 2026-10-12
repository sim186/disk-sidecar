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

	"github.com/sim186/disk-sidecar/internal/idx"
)

type copyReq struct {
	Items []qItem `json:"items"`
	To    qItem   `json:"to"` // the folder to copy into: a disk, and a path on it ("" is the root of the disk)
}

const partSuffix = ".sidecar-part"

// existingAncestor returns the deepest part of full that exists, and the part below it.
func existingAncestor(full string) (string, string) {
	rest := ""
	for p := full; ; p = filepath.Dir(p) {
		if _, err := os.Lstat(p); err == nil {
			return p, rest
		}
		if filepath.Dir(p) == p {
			return p, rest
		}
		rest = filepath.Join(filepath.Base(p), rest)
	}
}

// doCopy copies files and folders from any disk to a folder on any disk (the same one too). The
// originals are never touched. Every file is written under a temporary name, read back and
// compared with what was read from the source, and only then given its final name; the copies
// are added to the index of their disk so they show up at once.
func (s *Server) doCopy(req copyReq, job *Job) (any, error) {
	if len(req.Items) == 0 {
		return nil, refuse("nothing to copy")
	}
	plans, handles, _, done, err := s.planItems(req.Items, job, "copied")
	if err != nil {
		return nil, err
	}
	defer done()

	srcs, _ := s.listSources()
	var dst *Src
	for _, x := range srcs {
		if x.Label == req.To.Source {
			dst = x
		}
	}
	if dst == nil {
		return nil, refuse("unknown disk: %s", req.To.Source)
	}
	if dst.Kind != "disk" {
		return nil, refuse("%s is %s: nothing can be copied into it", dst.Label, readOnlyWhat(dst.Kind))
	}
	if !dirExists(dst.Root) {
		return nil, refuse("%s: the disk is not reachable from this computer (%s)", dst.Label, dst.Root)
	}
	destRel := strings.Trim(strings.ReplaceAll(req.To.Path, "\\", "/"), "/")
	if destRel != "" {
		if destRel, err = cleanRel(destRel); err != nil {
			return nil, err
		}
	}
	destDir := filepath.Join(dst.Root, filepath.FromSlash(destRel))
	anc, below := existingAncestor(destDir)
	if st, err := os.Stat(anc); err != nil || !st.IsDir() {
		return nil, refuse("%s: not a folder", destRel)
	}
	if realpath(anc) != realpath(dst.Root) && !inside(dst.Root, anc) {
		return nil, refuse("%s: outside the disk", destRel)
	}
	if err := s.guardPath(filepath.Join(realpath(anc), below)); err != nil {
		return nil, refuse("%s: %v", destRel, err)
	}
	if lst, err := os.Lstat(destDir); err == nil && lst.Mode()&os.ModeSymlink != 0 {
		return nil, refuse("%s: is a symbolic link", destRel)
	}

	// everything is checked before the first byte is written
	type job1 struct {
		m      *move
		target string // final folder or file
		base   string
	}
	var jobs []job1
	seen := map[string]bool{}
	var total int64
	for _, m := range plans {
		base := path.Base(m.rel)
		target := filepath.Join(destDir, base)
		if seen[strings.ToLower(base)] {
			return nil, refuse("two of the items are both called %q: copy them one at a time", base)
		}
		seen[strings.ToLower(base)] = true
		if _, err := os.Lstat(target); err == nil {
			return nil, refuse("%s: already exists in the destination; not overwriting", path.Join(destRel, base))
		}
		if m.isDir {
			rm, rt := realpath(m.full), filepath.Join(realpath(anc), below)
			if rt == rm || strings.HasPrefix(rt+string(filepath.Separator), rm+string(filepath.Separator)) {
				return nil, refuse("%s: cannot copy a folder into itself", m.rel)
			}
		}
		for _, r := range m.rows {
			total += r.size
		}
		jobs = append(jobs, job1{m, target, base})
	}
	if job != nil {
		job.Update("Copying…", 0, total)
	}

	ddb := handles[dst.DB]
	if ddb == nil {
		if ddb, err = idx.Open(dst.DB); err != nil {
			return nil, err
		}
		defer ddb.Close()
	}
	copyFile(dst.DB, dst.DB+".bak")
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
			row  moveRow
			rel  string
			sha  string
			body sql.NullString
		}
		var list []made
		fail := func(err error) (any, error) {
			os.RemoveAll(j.target) // the target did not exist before: all of it is ours
			return nil, refuse("%s: copy failed (%v). Nothing of it was kept. Copied so far: %d items.", m.rel, err, nitems)
		}
		for _, r := range m.rows {
			sub := "" // where this file goes under the target folder
			if m.isDir {
				sub = strings.TrimPrefix(r.path, m.rel+"/")
			}
			from := filepath.Join(m.root, filepath.FromSlash(r.path))
			to := j.target
			if m.isDir {
				to = filepath.Join(j.target, filepath.FromSlash(sub))
			}
			if job != nil {
				job.Phase("Copying " + path.Base(r.path))
			}
			sum, err := copyVerified(from, to, r, job)
			if err != nil {
				return fail(err)
			}
			if r.sha.Valid && r.sha.String != sum {
				os.Remove(to)
				return fail(fmt.Errorf("%s no longer matches the index: it was changed since the last scan", r.path))
			}
			rel := path.Join(destRel, j.base)
			if m.isDir {
				rel = path.Join(rel, sub)
			}
			var body sql.NullString
			m.db.QueryRow(`SELECT body FROM file_text WHERE rowid=?`, r.id).Scan(&body)
			list = append(list, made{r, rel, sum, body})
		}
		tx, err := ddb.Begin()
		if err != nil {
			return nil, err
		}
		for _, c := range list {
			name := path.Base(c.rel)
			ext := ""
			if i := strings.LastIndex(name, "."); i > 0 {
				ext = strings.ToLower(name[i+1:])
			}
			top := ""
			if i := strings.Index(c.rel, "/"); i >= 0 {
				top = c.rel[:i]
			}
			var id int64
			err := tx.QueryRow(`INSERT INTO files(source_id,path,name,ext,size,mtime,kind,top,sha256,seen) VALUES (?,?,?,?,?,?,?,?,?,1)
				ON CONFLICT(source_id,path) DO UPDATE SET size=excluded.size, mtime=excluded.mtime, kind=excluded.kind,
				top=excluded.top, sha256=excluded.sha256, seen=1 RETURNING id`,
				dst.SID, c.rel, name, ext, c.row.size, c.row.mtime, idx.KindOf(ext), top, c.sha).Scan(&id)
			if err == nil && c.body.Valid {
				tx.Exec(`DELETE FROM file_text WHERE rowid=?`, id)
				_, err = tx.Exec(`INSERT INTO file_text(rowid, path, body) VALUES (?,?,?)`, id, c.rel, c.body.String)
			}
			if err != nil {
				tx.Rollback()
				return nil, fmt.Errorf("the files were copied but the index was not updated (%v); use Check now", err)
			}
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		var ibytes int64
		for _, c := range list { // the source learns its hash too: the copy is now a proven duplicate
			if !c.row.sha.Valid {
				m.db.Exec(`UPDATE files SET sha256=? WHERE id=? AND sha256 IS NULL`, c.sha, c.row.id)
			}
			ibytes += c.row.size
		}
		nbytes += ibytes
		nfiles += len(list)
		nitems++
		s.logAction(m.disk, "copy-from", m.label, m.rel, len(list), 0)
		s.logAction(dst.Disk, "copy-to", dst.Label, path.Join(destRel, j.base), len(list), ibytes)
	}
	return map[string]any{"copied": nitems, "files": nfiles, "bytes": nbytes}, nil
}

// copyVerified copies from to to (which must not exist) through a temporary file, checks what
// was written by reading it back, restores the modification time and returns the SHA-256.
func copyVerified(from, to string, r moveRow, job *Job) (string, error) {
	in, err := os.Open(from)
	if err != nil {
		return "", err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return "", err
	}
	if st.Size() != r.size || st.ModTime().Unix() != r.mtime {
		return "", fmt.Errorf("%s changed since the last scan", r.path)
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
	if n != r.size {
		return "", fmt.Errorf("%s changed while it was being copied", r.path)
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
		return "", fmt.Errorf("%s was not written correctly (the copy does not match)", r.path)
	}
	mt := time.Unix(r.mtime, 0)
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
