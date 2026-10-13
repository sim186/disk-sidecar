// Package catalog is what Sidecar can see right now: the plugged-in disks that carry an index,
// the snapshots of other computers they hold, and the phones catalogued on this computer. It
// answers "which source is this label, and may I change it?" for everything that acts on files.
package catalog

import (
	"crypto/sha1"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/sim186/disk-sidecar/internal/idx"
	"github.com/sim186/disk-sidecar/internal/phone"
	"github.com/sim186/disk-sidecar/internal/refusal"
)

// Options says where to look for disks.
type Options struct {
	HomeDB          string   // use this index as the home disk (development); default: the disk the program is on
	Mounts          []string // extra folders that contain mounted disks
	NoDefaultMounts bool
}

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

// Log adds a line to the disk's log of what Sidecar changed on it.
func (d *Disk) Log(action, source, path string, nfiles int, nbytes int64) {
	p := filepath.Join(d.Dir(), "log.csv")
	_, statErr := os.Stat(p)
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	w := csv.NewWriter(f)
	if statErr != nil {
		w.Write([]string{"time", "action", "source", "path", "files", "bytes"})
	}
	w.Write([]string{time.Now().Format("2006-01-02 15:04:05"), action, source, path, fmt.Sprint(nfiles), fmt.Sprint(nbytes)})
	w.Flush()
}

// Backup copies the index next to itself before a change, as index.db.bak.
func (d *Disk) Backup() error { return CopyFile(d.DB, d.DB+".bak") }

// Source is one source of files (a disk, or a snapshot of a computer, or a phone) visible now.
type Source struct {
	Label     string // unique across everything visible
	Raw       string // the label in its own index
	DB        string
	SID       int64
	Root      string // where it is on this computer (a disk), or was when scanned
	Kind      string // "disk", "snapshot" (another computer) or "phone"
	ScannedAt int64
	Disk      *Disk // the disk it lives on; nil for snapshots and phones
}

// Reachable reports whether the files of the source are on this computer now.
func (x *Source) Reachable() bool { return x.Kind == "disk" && DirExists(x.Root) }

// ReadOnlyWhat names a source that cannot be changed, for the messages that refuse to change it.
func (x *Source) ReadOnlyWhat() string {
	if x.Kind == "phone" {
		return "a phone, which Sidecar only reads"
	}
	return "a read-only snapshot"
}

// Catalog is everything visible at one moment.
type Catalog struct {
	sources []*Source
	disks   []*Disk
}

// Sources lists every source, disks first, with labels unique across all of them.
func (c *Catalog) Sources() []*Source { return c.sources }

// Disks lists the plugged-in disks; the home disk comes first.
func (c *Catalog) Disks() []*Disk { return c.disks }

// Find returns the source called label, or nil.
func (c *Catalog) Find(label string) *Source {
	for _, x := range c.sources {
		if x.Label == label {
			return x
		}
	}
	return nil
}

// Get returns the source called label, or refuses.
func (c *Catalog) Get(label string) (*Source, error) {
	if x := c.Find(label); x != nil {
		return x, nil
	}
	return nil, refusal.New("unknown source: %s", label)
}

// Writable returns the source called label if it is a disk, whose files Sidecar may change. If
// not, it refuses, saying why, followed by what that means (e.g. "its files cannot be moved").
func (c *Catalog) Writable(label, meaning string) (*Source, error) {
	x, err := c.Get(label)
	if err != nil {
		return nil, err
	}
	if x.Kind != "disk" {
		return nil, refusal.New("%s is %s: %s", x.Label, x.ReadOnlyWhat(), meaning)
	}
	return x, nil
}

// Disk returns the plugged-in disk with this key, or nil.
func (c *Catalog) Disk(key string) *Disk {
	for _, d := range c.disks {
		if d.Key == key {
			return d
		}
	}
	return nil
}

// In returns the sources kept in the index db.
func (c *Catalog) In(db string) []*Source {
	var out []*Source
	for _, x := range c.sources {
		if x.DB == db {
			out = append(out, x)
		}
	}
	return out
}

// Source returns the source sid of the index db, or nil.
func (c *Catalog) Source(db string, sid int64) *Source {
	for _, x := range c.sources {
		if x.DB == db && x.SID == sid {
			return x
		}
	}
	return nil
}

// Indexes lists every index file that holds a source, once each, in order.
func (c *Catalog) Indexes() []string {
	var out []string
	seen := map[string]bool{}
	for _, x := range c.sources {
		if !seen[x.DB] {
			seen[x.DB] = true
			out = append(out, x.DB)
		}
	}
	return out
}

// MountCandidates lists the folders where a disk could be mounted.
func MountCandidates(o Options) []string {
	var out []string
	var bases []string
	if !o.NoDefaultMounts {
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
	bases = append(bases, o.Mounts...) // --mounts works on every system
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

// Discover lists every disk that carries an index; the home disk comes first.
func Discover(o Options) []*Disk {
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
	if o.HomeDB != "" {
		add(filepath.Dir(o.HomeDB), o.HomeDB)
	} else if root, db := exeDisk(); db != "" {
		add(root, db)
	}
	for _, p := range MountCandidates(o) {
		add(p, filepath.Join(p, ".sidecar", "index.db"))
	}
	if h, err := os.UserHomeDir(); err == nil && h != "" && !o.NoDefaultMounts { // this computer, once it has been set up
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

type row struct {
	id          int64
	label, root string
	at          int64
}

func sourceRows(path string, each func(db idx.Querier, r row)) {
	db, err := idx.OpenRO(path)
	if err != nil {
		return
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, label, COALESCE(root,''), COALESCE(scanned_at,0) FROM sources ORDER BY id`)
	if err != nil {
		return
	}
	var rs []row
	for rows.Next() {
		var x row
		if rows.Scan(&x.id, &x.label, &x.root, &x.at) == nil {
			rs = append(rs, x)
		}
	}
	rows.Close()
	for _, r := range rs {
		each(db, r)
	}
}

// Load looks at everything visible now.
func Load(o Options) *Catalog {
	c := &Catalog{disks: Discover(o)}
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
	for _, d := range c.disks {
		hint := filepath.Base(d.Root)
		if hint == "" || hint == "." {
			hint = "disk"
		}
		sourceRows(d.DB, func(db idx.Querier, x row) {
			real := idx.ResolveRoot(db, x.id, x.root, d.Root)
			c.sources = append(c.sources, &Source{Label: uniq(x.label, hint), Raw: x.label, DB: d.DB, SID: x.id, Root: real, Kind: "disk", ScannedAt: x.at, Disk: d})
		})
	}
	for _, f := range peerDBs(c.disks) {
		sourceRows(f, func(_ idx.Querier, x row) {
			c.sources = append(c.sources, &Source{Label: uniq(x.label, "snapshot"), Raw: x.label, DB: f, SID: x.id, Root: x.root, Kind: "snapshot", ScannedAt: x.at})
		})
	}
	if dir, err := phone.Dir(); err == nil { // phones: catalogued from this computer, read only
		files, _ := filepath.Glob(filepath.Join(dir, "*.db"))
		sort.Strings(files)
		for _, f := range files {
			sourceRows(f, func(_ idx.Querier, x row) {
				c.sources = append(c.sources, &Source{Label: uniq(x.label, "phone"), Raw: x.label, DB: f, SID: x.id, Kind: "phone", ScannedAt: x.at})
			})
		}
	}
	return c
}

// CopyFile copies the file src to dst, replacing it.
func CopyFile(src, dst string) error {
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
