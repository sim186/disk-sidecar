package server

import (
	"database/sql"
	"os"
	"path/filepath"

	"github.com/sim186/disk-sidecar/internal/catalog"
	"github.com/sim186/disk-sidecar/internal/farewell"
	"github.com/sim186/disk-sidecar/internal/idx"
	"github.com/sim186/disk-sidecar/internal/phone"
	"github.com/sim186/disk-sidecar/internal/scan"
	"github.com/sim186/disk-sidecar/internal/transfer"
)

// An Android phone is copied from and, once its files are proven safe on a disk, released. The
// phone has no farewell box, so the only protection is proof: a file is removed from the phone
// only after the phone and a disk have both just been read and gave the same SHA-256.

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

func oneSource(items []farewell.Item) (string, error) {
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
func (s *Server) doPhoneRelease(items []farewell.Item, confirm string, dry bool, job *Job) (any, error) {
	if !dry && !farewell.Confirmed(confirm) {
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
	picked, err := transfer.PhoneItems(pdb, psrc, items)
	if err != nil {
		return nil, err
	}
	var rows []idx.File
	for _, it := range picked {
		rows = append(rows, it.Files...)
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
		remote := phone.RemotePath(r.Path)
		sum, err := phone.RemoteSHA(dev.Serial, remote)
		if err != nil {
			return nil, refuse("%v", err)
		}
		if r.SHA.Valid && r.SHA.String != sum {
			nkeep++
			kept += r.Size
			unsafe = append(unsafe, r.Path+" (changed on the phone)")
			continue
		}
		where := backupOf(disks, dbs, r.Size, sum)
		if where == "" {
			nkeep++
			kept += r.Size
			if len(unsafe) < 5 {
				unsafe = append(unsafe, r.Path)
			}
			continue
		}
		if dry {
			nfree++
			freed += r.Size
			if len(sample) < 3 {
				sample = append(sample, r.Path+" = "+where)
			}
			continue
		}
		if err := phone.Remove(dev.Serial, remote); err != nil {
			return nil, refuse("%v. Released so far: %d files.", err, nfree)
		}
		idx.RemoveFile(pdb, r.ID)
		nfree++
		freed += r.Size
	}
	return map[string]any{"dry": dry, "release": nfree, "bytes": freed, "kept": nkeep, "kept_bytes": kept,
		"sample": sample, "unsafe": unsafe}, nil
}
