package server

import (
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/sim186/disk-sidecar/internal/idx"
	"github.com/sim186/disk-sidecar/internal/phone"
)

// Disk is a plugged-in disk that carries an index.
type Disk struct {
	Root string // the folder that holds .sidecar (used to find the disk again under another path)
	DB   string
	Key  string
}

func newDisk(root, db string) *Disk {
	h := sha1.Sum([]byte(db))
	return &Disk{Root: root, DB: db, Key: hex.EncodeToString(h[:])[:8]}
}

// Dir is the .sidecar folder of the disk.
func (d *Disk) Dir() string { return filepath.Dir(d.DB) }

// Src is one source of files (a disk, or a snapshot of a computer) visible now.
type Src struct {
	Label, Raw, DB string
	SID            int64
	Root, Kind     string // Kind: "disk", "snapshot" (another computer) or "phone"
	ScannedAt      int64
	Disk           *Disk
}

func (s *Server) mountCandidates() []string {
	var out []string
	var bases []string
	if !s.cfg.NoDefaultMounts {
		if runtime.GOOS == "windows" {
			for c := 'A'; c <= 'Z'; c++ { // each drive letter is itself a disk
				p := string(c) + `:\`
				if _, err := os.Stat(p); err == nil {
					out = append(out, p)
				}
			}
		} else {
			bases = append(bases, "/Volumes", "/media", "/mnt", "/run/media")
		}
	}
	bases = append(bases, s.cfg.Mounts...) // --mounts works on every system
	for _, b := range bases {
		ents, err := os.ReadDir(b)
		if err != nil {
			continue
		}
		for _, e := range ents {
			p := filepath.Join(b, e.Name())
			out = append(out, p)
			if b == "/run/media" { // /run/media/<user>/<disk>
				if subs, err := os.ReadDir(p); err == nil {
					for _, sub := range subs {
						out = append(out, filepath.Join(p, sub.Name()))
					}
				}
			}
		}
	}
	return out
}

// exeDisk finds the disk the running program is installed on (<disk>/.sidecar/bin/...).
func exeDisk() (root, db string) {
	exe, err := os.Executable()
	if err != nil {
		return "", ""
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	for dir := filepath.Dir(exe); ; dir = filepath.Dir(dir) {
		if filepath.Base(dir) == ".sidecar" {
			return filepath.Dir(dir), filepath.Join(dir, "index.db")
		}
		if filepath.Dir(dir) == dir {
			return "", ""
		}
	}
}

// discover lists every disk that carries an index; the home disk comes first.
func (s *Server) discover() []*Disk {
	var disks []*Disk
	seen := map[string]bool{}
	add := func(root, db string) {
		if st, err := os.Stat(db); err != nil || !st.Mode().IsRegular() {
			return
		}
		real, err := filepath.EvalSymlinks(db)
		if err != nil {
			real = db
		}
		if seen[real] {
			return
		}
		seen[real] = true
		disks = append(disks, newDisk(root, db))
	}
	if s.cfg.HomeDB != "" {
		add(filepath.Dir(s.cfg.HomeDB), s.cfg.HomeDB)
	} else if root, db := exeDisk(); db != "" {
		add(root, db)
	}
	for _, p := range s.mountCandidates() {
		add(p, filepath.Join(p, ".sidecar", "index.db"))
	}
	if h, err := os.UserHomeDir(); err == nil && h != "" && !s.cfg.NoDefaultMounts { // this computer, once it has been set up
		add(h, filepath.Join(h, ".sidecar", "index.db"))
	}
	return disks
}

func peerDBs(disks []*Disk) []string {
	var out []string
	seen := map[string]bool{}
	for _, d := range disks {
		files, _ := filepath.Glob(filepath.Join(d.Dir(), "peers", "*.db"))
		sort.Strings(files)
		for _, f := range files {
			real, err := filepath.EvalSymlinks(f)
			if err != nil {
				real = f
			}
			if !seen[real] {
				seen[real] = true
				out = append(out, f)
			}
		}
	}
	return out
}

// listSources returns every source visible now, with labels that are unique across disks and
// snapshots, and the plugged-in disks.
func (s *Server) listSources() ([]*Src, []*Disk) {
	var srcs []*Src
	used := map[string]bool{}
	uniq := func(label, hint string) string {
		l := label
		if used[l] {
			l = label + " (" + hint + ")"
		}
		for used[l] {
			l += "'"
		}
		used[l] = true
		return l
	}
	disks := s.discover()
	for _, d := range disks {
		db, err := idx.OpenRO(d.DB)
		if err != nil {
			continue
		}
		rows, err := db.Query(`SELECT id, label, COALESCE(root,''), COALESCE(scanned_at,0) FROM sources ORDER BY id`)
		if err == nil {
			type r struct {
				id          int64
				label, root string
				at          int64
			}
			var rs []r
			for rows.Next() {
				var x r
				if rows.Scan(&x.id, &x.label, &x.root, &x.at) == nil {
					rs = append(rs, x)
				}
			}
			rows.Close()
			hint := filepath.Base(d.Root)
			if hint == "" || hint == "." {
				hint = "disk"
			}
			for _, x := range rs {
				real := idx.ResolveRoot(db, x.id, x.root, d.Root)
				srcs = append(srcs, &Src{Label: uniq(x.label, hint), Raw: x.label, DB: d.DB, SID: x.id, Root: real, Kind: "disk", ScannedAt: x.at, Disk: d})
			}
		}
		db.Close()
	}
	for _, f := range peerDBs(disks) {
		db, err := idx.OpenRO(f)
		if err != nil {
			continue
		}
		rows, err := db.Query(`SELECT id, label, COALESCE(root,''), COALESCE(scanned_at,0) FROM sources ORDER BY id`)
		if err == nil {
			for rows.Next() {
				var id, at int64
				var label, root string
				if rows.Scan(&id, &label, &root, &at) == nil {
					srcs = append(srcs, &Src{Label: uniq(label, "snapshot"), Raw: label, DB: f, SID: id, Root: root, Kind: "snapshot", ScannedAt: at})
				}
			}
			rows.Close()
		}
		db.Close()
	}
	if dir, err := phone.Dir(); err == nil { // phones: catalogued from this computer, read only
		files, _ := filepath.Glob(filepath.Join(dir, "*.db"))
		sort.Strings(files)
		for _, f := range files {
			db, err := idx.OpenRO(f)
			if err != nil {
				continue
			}
			rows, err := db.Query(`SELECT id, label, COALESCE(scanned_at,0) FROM sources ORDER BY id`)
			if err == nil {
				for rows.Next() {
					var id, at int64
					var label string
					if rows.Scan(&id, &label, &at) == nil {
						srcs = append(srcs, &Src{Label: uniq(label, "phone"), Raw: label, DB: f, SID: id, Kind: "phone", ScannedAt: at})
					}
				}
				rows.Close()
			}
			db.Close()
		}
	}
	return srcs, disks
}

func diskIDOf(dbPath string) string {
	db, err := idx.OpenRO(dbPath)
	if err != nil {
		return ""
	}
	defer db.Close()
	var v sql.NullString
	db.QueryRow(`SELECT value FROM meta WHERE key='disk_id'`).Scan(&v)
	return v.String
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

// readOnlyWhat names a source that cannot be changed, for the messages that refuse to change it.
func readOnlyWhat(kind string) string {
	if kind == "phone" {
		return "a phone, which Sidecar only reads"
	}
	return "a read-only snapshot"
}
