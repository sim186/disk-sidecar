package transfer

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/sim186/disk-sidecar/internal/catalog"
	"github.com/sim186/disk-sidecar/internal/farewell"
	"github.com/sim186/disk-sidecar/internal/idx"
	"github.com/sim186/disk-sidecar/internal/phone"
	"github.com/sim186/disk-sidecar/internal/refusal"
	"github.com/sim186/disk-sidecar/internal/scan"
)

// PhoneItems returns what the items hold according to the phone's catalog db: a file, or a
// folder and all that is inside. A file chosen twice is copied once.
func PhoneItems(db *sql.DB, src *catalog.Source, items []farewell.Item) ([]Item, error) {
	var out []Item
	seen := map[int64]bool{}
	for _, it := range items {
		rel, err := catalog.CleanRel(it.Path)
		if err != nil {
			return nil, err
		}
		files, err := idx.FilesAtOrUnder(db, src.SID, rel)
		if err != nil {
			return nil, err
		}
		if len(files) == 0 {
			return nil, refusal.New("%s: not in the catalog of the phone", it.Path)
		}
		item := Item{Source: src.Label, Rel: rel}
		for _, f := range files {
			item.IsDir = item.IsDir || f.Path != rel
			if !seen[f.ID] {
				seen[f.ID] = true
				item.Files = append(item.Files, f)
			}
		}
		if len(item.Files) > 0 {
			out = append(out, item)
		}
	}
	return out, nil
}

// Phone is an Android phone read through adb. A file is proven when the hash of what arrived
// equals the hash the phone itself reads.
type Phone struct {
	DB     *sql.DB // its catalog on this computer
	Serial string
	// Pull copies a file of the phone to a local path; SHA is the phone's own SHA-256 of a file.
	// They talk to adb; tests replace them.
	Pull func(serial, remote, local string) error
	SHA  func(serial, remote string) (string, error)
}

// FromPhone is the origin for the phone whose catalog is db.
func FromPhone(db *sql.DB, serial string) *Phone {
	return &Phone{DB: db, Serial: serial, Pull: phone.Pull, SHA: phone.RemoteSHA}
}

func (ph *Phone) Fetch(it Item, f idx.File, to string, p farewell.Progress) (string, error) {
	remote := phone.RemotePath(f.Path)
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return "", err
	}
	if _, err := os.Lstat(to); err == nil {
		return "", fmt.Errorf("%s already exists", to)
	}
	tmp := to + PartSuffix
	os.Remove(tmp)
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmp)
		}
	}()
	if err := ph.Pull(ph.Serial, remote, tmp); err != nil {
		return "", err
	}
	st, err := os.Stat(tmp)
	if err != nil {
		return "", err
	}
	sum, err := scan.SHA256(tmp)
	if err != nil {
		return "", err
	}
	remoteSum, err := ph.SHA(ph.Serial, remote)
	if err != nil {
		return "", err
	}
	if sum != remoteSum {
		return "", fmt.Errorf("%s does not match what the phone holds (%d bytes arrived)", f.Path, st.Size())
	}
	mt := time.Unix(f.Mtime, 0)
	os.Chtimes(tmp, mt, mt)
	if _, err := os.Lstat(to); err == nil {
		return "", fmt.Errorf("%s already exists", to)
	}
	if err := os.Rename(tmp, to); err != nil {
		return "", err
	}
	ok = true
	p.Advance(f.Size)
	return sum, nil
}

func (ph *Phone) Text(Item, idx.File) sql.NullString { return sql.NullString{} }

// Copied records the proven hashes in the phone's catalog: a copy of each file now exists.
func (ph *Phone) Copied(it Item, sums map[int64]string) {
	for _, f := range it.Files {
		idx.SetHash(ph.DB, f.ID, sums[f.ID])
	}
}
