package server

import (
	"database/sql"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/sim186/disk-sidecar/internal/catalog"
	"github.com/sim186/disk-sidecar/internal/idx"
	"github.com/sim186/disk-sidecar/internal/phone"
	"github.com/sim186/disk-sidecar/internal/scan"
)

// letGoWords is what the user types to release files for good (from the farewell box or a phone), in the spirit of Marie Kondo.
const letGoWords = "let it go"

// letGo reports whether typed is the confirmation, ignoring case and surrounding spaces.
func letGo(typed string) bool { return strings.ToLower(strings.TrimSpace(typed)) == letGoWords }

// An Android phone is copied from and, once its files are proven safe on a disk, released. The
// phone has no farewell box, so the only protection is proof: a file is removed from the phone
// only after the phone and a disk have both just been read and gave the same SHA-256.

type prow struct {
	id, size, mtime int64
	path            string
	sha             sql.NullString
}

// phoneDevice finds the catalog named label and the connected phone it describes.
func (s *Server) phoneDevice(label string) (*catalog.Source, phone.Device, error) {
	x, err := s.catalog().Get(label)
	if err != nil {
		return nil, phone.Device{}, err
	}
	if x.Kind != "phone" {
		return nil, phone.Device{}, refuse("%s is not a phone", label)
	}
	plat, serial, ok := phone.Identity(x.DB)
	if !ok {
		return nil, phone.Device{}, refuse("%s: read the phone again with Add a phone… first", label)
	}
	if plat != "android" {
		return nil, phone.Device{}, refuse("%s: only Android phones can be copied from or released; an iPhone is read only", label)
	}
	dev, found := phone.Find(serial)
	if !found || dev.State != "ready" {
		return nil, phone.Device{}, refuse("%s is not connected and unlocked", label)
	}
	return x, dev, nil
}

// phoneRows returns the files held by the items (a file, or a folder and all that is inside).
// folder[rel] tells which items were folders.
func phoneRows(db *sql.DB, sid int64, items []qItem) (rows []prow, itemOf map[int64]string, folder map[string]bool, err error) {
	itemOf, folder = map[int64]string{}, map[string]bool{}
	seen := map[int64]bool{}
	for _, it := range items {
		rel, err := catalog.CleanRel(it.Path)
		if err != nil {
			return nil, nil, nil, err
		}
		rs, err := db.Query(`SELECT id, path, size, mtime, sha256 FROM files WHERE source_id=? AND (path=? OR path LIKE ? ESCAPE '\')`,
			sid, rel, idx.LikePrefix(rel))
		if err != nil {
			return nil, nil, nil, err
		}
		n := 0
		for rs.Next() {
			var r prow
			if rs.Scan(&r.id, &r.path, &r.size, &r.mtime, &r.sha) != nil {
				continue
			}
			n++
			if r.path != rel {
				folder[rel] = true
			}
			if !seen[r.id] {
				seen[r.id] = true
				itemOf[r.id] = rel
				rows = append(rows, r)
			}
		}
		rs.Close()
		if n == 0 {
			return nil, nil, nil, refuse("%s: not in the catalog of the phone", it.Path)
		}
	}
	return rows, itemOf, folder, nil
}

func oneSource(items []qItem) (string, error) {
	if len(items) == 0 {
		return "", refuse("nothing selected")
	}
	for _, it := range items {
		if it.Source != items[0].Source {
			return "", refuse("choose files from one phone at a time")
		}
	}
	return items[0].Source, nil
}

// doPhoneCopy copies files from an Android phone to a folder on a disk. Each file is pulled under
// a temporary name, hashed, compared with the hash the phone itself reports, and only then given
// its name. The phone is not touched.
func (s *Server) doPhoneCopy(req copyReq, job *Job) (any, error) {
	label, err := oneSource(req.Items)
	if err != nil {
		return nil, err
	}
	psrc, dev, err := s.phoneDevice(label)
	if err != nil {
		return nil, err
	}
	srcs := s.catalog().Sources()
	var dst *catalog.Source
	for _, x := range srcs {
		if x.Label == req.To.Source {
			dst = x
		}
	}
	if dst == nil || dst.Kind != "disk" {
		return nil, refuse("choose a disk to copy to")
	}
	if !catalog.DirExists(dst.Root) {
		return nil, refuse("%s: the disk is not reachable from this computer", dst.Label)
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

	pdb, err := idx.Open(psrc.DB)
	if err != nil {
		return nil, err
	}
	defer pdb.Close()
	rows, itemOf, _, err := phoneRows(pdb, psrc.SID, req.Items)
	if err != nil {
		return nil, err
	}
	var total int64
	bases := map[string]string{} // item -> name it gets on the disk
	taken := map[string]bool{}
	for _, r := range rows {
		total += r.size
		it := itemOf[r.id]
		if _, ok := bases[it]; ok {
			continue
		}
		b := path.Base(it)
		if taken[strings.ToLower(b)] {
			return nil, refuse("two of the items are both called %q: copy them one at a time", b)
		}
		taken[strings.ToLower(b)] = true
		if _, err := os.Lstat(filepath.Join(destDir, b)); err == nil {
			return nil, refuse("%s: already exists in the destination; not overwriting", path.Join(destRel, b))
		}
		bases[it] = b
	}
	ddb, err := idx.Open(dst.DB)
	if err != nil {
		return nil, err
	}
	defer ddb.Close()
	catalog.CopyFile(dst.DB, dst.DB+".bak")
	if job != nil {
		job.Update("Copying from "+dev.Name+"…", 0, total)
	}

	type made struct {
		r      prow
		rel    string
		sha    string
		target string
	}
	byItem := map[string][]made{}
	var order []string
	undo := func() { // nothing of a failed copy is kept
		for _, ms := range byItem {
			for _, m := range ms {
				os.Remove(m.target)
			}
		}
	}
	for _, r := range rows {
		it := itemOf[r.id]
		if _, ok := byItem[it]; !ok {
			order = append(order, it)
			byItem[it] = nil
		}
		rel := path.Join(destRel, bases[it])
		if r.path != it {
			rel = path.Join(rel, strings.TrimPrefix(r.path, it+"/"))
		}
		to := filepath.Join(dst.Root, filepath.FromSlash(rel))
		if job != nil {
			job.Phase("Copying " + path.Base(r.path))
		}
		sum, err := pullVerified(dev.Serial, phone.RemotePath(r.path), to, r)
		if err != nil {
			undo()
			return nil, refuse("%s: copy failed (%v). Nothing of this copy was kept.", it, err)
		}
		byItem[it] = append(byItem[it], made{r, rel, sum, to})
		if job != nil {
			job.Advance(r.size)
		}
	}
	var nfiles int
	var nbytes int64
	for _, it := range order {
		tx, err := ddb.Begin()
		if err != nil {
			return nil, err
		}
		fw, err := idx.NewFileWriter(tx, dst.SID)
		if err != nil {
			tx.Rollback()
			return nil, err
		}
		for _, c := range byItem[it] {
			if _, err := fw.Put(c.rel, c.r.size, c.r.mtime, sql.NullString{String: c.sha, Valid: true}); err != nil {
				fw.Close()
				tx.Rollback()
				return nil, fmt.Errorf("the files were copied but the index was not updated (%v); use Check now", err)
			}
		}
		fw.Close()
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		for _, c := range byItem[it] { // the phone's file now has a proven hash: a copy exists
			idx.SetHash(pdb, c.r.id, c.sha)
			nbytes += c.r.size
			nfiles++
		}
		dst.Disk.Log("copy-to", dst.Label, path.Join(destRel, bases[it])+" (from "+psrc.Label+")", len(byItem[it]), 0)
	}
	return map[string]any{"copied": len(order), "files": nfiles, "bytes": nbytes}, nil
}

// pullVerified pulls remote to the final path `to` (which must not exist) and returns its SHA-256
// after checking it against the phone's own reading of the file.
func pullVerified(serial, remote, to string, r prow) (string, error) {
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return "", err
	}
	if _, err := os.Lstat(to); err == nil {
		return "", fmt.Errorf("%s already exists", to)
	}
	tmp := to + partSuffix
	os.Remove(tmp)
	if err := phone.Pull(serial, remote, tmp); err != nil {
		os.Remove(tmp)
		return "", err
	}
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmp)
		}
	}()
	st, err := os.Stat(tmp)
	if err != nil {
		return "", err
	}
	sum, err := scan.SHA256(tmp)
	if err != nil {
		return "", err
	}
	remoteSum, err := phone.RemoteSHA(serial, remote)
	if err != nil {
		return "", err
	}
	if sum != remoteSum {
		return "", fmt.Errorf("%s does not match what the phone holds (%d bytes arrived)", r.path, st.Size())
	}
	if r.sha.Valid && r.sha.String != sum {
		return "", fmt.Errorf("%s changed on the phone since it was last hashed", r.path)
	}
	mt := time.Unix(r.mtime, 0)
	os.Chtimes(tmp, mt, mt)
	if _, err := os.Lstat(to); err == nil {
		return "", fmt.Errorf("%s already exists", to)
	}
	if err := os.Rename(tmp, to); err != nil {
		return "", err
	}
	ok = true
	return sum, nil
}

// backupOf looks for a copy of a phone file on a connected disk and proves it: the disk file is
// read now and must have the hash. It returns where the proof was found, or "".
func backupOf(disks []*catalog.Source, dbs map[string]*sql.DB, size int64, sum string) string {
	for _, d := range disks {
		db := dbs[d.DB]
		if db == nil {
			continue
		}
		rs, err := db.Query(`SELECT path, sha256 FROM files WHERE source_id=? AND size=? LIMIT 50`, d.SID, size)
		if err != nil {
			continue
		}
		type cand struct {
			p   string
			sha sql.NullString
		}
		var cs []cand
		for rs.Next() {
			var c cand
			if rs.Scan(&c.p, &c.sha) == nil && (!c.sha.Valid || c.sha.String == sum) {
				cs = append(cs, c)
			}
		}
		rs.Close()
		for _, c := range cs {
			full := filepath.Join(d.Root, filepath.FromSlash(c.p))
			if st, err := os.Stat(full); err != nil || !st.Mode().IsRegular() || st.Size() != size {
				continue
			}
			if h, err := scan.SHA256(full); err == nil && h == sum {
				return d.Label + ":" + c.p
			}
		}
	}
	return ""
}

// doPhoneRelease deletes files from an Android phone. Dry only reports. A file goes only if the
// phone and a connected disk, both read just now, agree on its SHA-256.
func (s *Server) doPhoneRelease(items []qItem, confirm string, dry bool, job *Job) (any, error) {
	if !dry && !letGo(confirm) {
		return nil, refuse("confirmation missing")
	}
	label, err := oneSource(items)
	if err != nil {
		return nil, err
	}
	psrc, dev, err := s.phoneDevice(label)
	if err != nil {
		return nil, err
	}
	pdb, err := idx.Open(psrc.DB)
	if err != nil {
		return nil, err
	}
	defer pdb.Close()
	rows, _, _, err := phoneRows(pdb, psrc.SID, items)
	if err != nil {
		return nil, err
	}
	var disks []*catalog.Source
	dbs := map[string]*sql.DB{}
	for _, x := range s.catalog().Sources() {
		if x.Reachable() {
			if _, ok := dbs[x.DB]; !ok {
				db, err := idx.OpenRO(x.DB)
				if err != nil {
					continue
				}
				defer db.Close()
				dbs[x.DB] = db
			}
			disks = append(disks, x)
		}
	}
	if len(disks) == 0 {
		return nil, refuse("no disk is connected, so nothing on the phone can be proven safe")
	}
	if job != nil {
		job.Update("Checking the copies on your disks…", 0, int64(len(rows)))
	}
	var freed, kept int64
	var nfree, nkeep int
	var sample []string
	var unsafe []string
	for _, r := range rows {
		if job != nil {
			job.Advance(1)
		}
		remote := phone.RemotePath(r.path)
		sum, err := phone.RemoteSHA(dev.Serial, remote)
		if err != nil {
			return nil, refuse("%v", err)
		}
		if r.sha.Valid && r.sha.String != sum {
			nkeep++
			kept += r.size
			unsafe = append(unsafe, r.path+" (changed on the phone)")
			continue
		}
		where := backupOf(disks, dbs, r.size, sum)
		if where == "" {
			nkeep++
			kept += r.size
			if len(unsafe) < 5 {
				unsafe = append(unsafe, r.path)
			}
			continue
		}
		if dry {
			nfree++
			freed += r.size
			if len(sample) < 3 {
				sample = append(sample, r.path+" = "+where)
			}
			continue
		}
		if err := phone.Remove(dev.Serial, remote); err != nil {
			return nil, refuse("%v. Released so far: %d files.", err, nfree)
		}
		idx.RemoveFile(pdb, r.id)
		nfree++
		freed += r.size
	}
	return map[string]any{"dry": dry, "release": nfree, "bytes": freed, "kept": nkeep, "kept_bytes": kept,
		"sample": sample, "unsafe": unsafe}, nil
}
