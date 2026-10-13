// Package farewell is the farewell box: files the user lets go of are moved to a quarantine
// folder on their own disk, from where they can be restored, until the box is emptied for good.
// It holds every rule that protects files: nothing that changed since the scan, never the last
// copy of a duplicated file, nothing the guard refuses, nothing outside its disk.
package farewell

import (
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/sim186/disk-sidecar/internal/catalog"
	"github.com/sim186/disk-sidecar/internal/idx"
	"github.com/sim186/disk-sidecar/internal/refusal"
)

// Dir is where the farewell box is, relative to the root of each disk.
const Dir = ".sidecar/_quarantine"

// Words is what the user types to release files for good, in the spirit of Marie Kondo.
const Words = "let it go"

// Confirmed reports whether typed is the confirmation, ignoring case and surrounding spaces.
func Confirmed(typed string) bool { return strings.ToLower(strings.TrimSpace(typed)) == Words }

// Progress is told how a long action goes. A server job satisfies it.
type Progress interface {
	Update(phase string, done, total int64) // a negative number leaves that value as it is
	Phase(p string)
	SetDone(n int64)
	Advance(n int64)
}

type silent struct{}

func (silent) Update(string, int64, int64) {}
func (silent) Phase(string)                {}
func (silent) SetDone(int64)               {}
func (silent) Advance(int64)               {}

// Item is a file or folder chosen on the page: a source label and a path relative to its root.
type Item struct {
	Source string `json:"source"`
	Path   string `json:"path"`
}

// Box acts on the files of the disks in Catalog.
type Box struct {
	Catalog *catalog.Catalog
	// Guard says whether a path on this computer may be changed; nil allows everything inside a disk.
	Guard    func(full string) error
	Progress Progress // nil: silent
}

func (b Box) progress() Progress {
	if b.Progress == nil {
		return silent{}
	}
	return b.Progress
}

func (b Box) guard(full string) error {
	if b.Guard == nil {
		return nil
	}
	return b.Guard(full)
}

// Pick is a checked item: it exists, is as the index says, and may be changed.
type Pick struct {
	Source *catalog.Source
	Rel    string // relative to the source root
	Full   string // on this computer
	IsDir  bool
	Files  []idx.File // what it holds
	DB     *sql.DB    // the index of its disk, open for writing until the selection is closed
}

// Selection is a set of checked items, with the indexes of their disks open.
type Selection struct {
	Picks []*Pick
	dbs   map[string]*sql.DB
}

// Index returns the open index at path, or nil if no item lives there.
func (s *Selection) Index(path string) *sql.DB { return s.dbs[path] }

// Close closes the indexes.
func (s *Selection) Close() {
	for _, db := range s.dbs {
		db.Close()
	}
}

// Check validates items that live on any number of disks and returns what they hold. verb says
// what is about to happen to them, for the refusals ("moved", "copied"). Close the selection.
func (b Box) Check(items []Item, verb string) (*Selection, error) {
	sel := &Selection{dbs: map[string]*sql.DB{}}
	type want struct {
		src *catalog.Source
		rel string
	}
	var wants []want
	for _, it := range items {
		src, err := b.Catalog.Writable(it.Source, "its files cannot be "+verb)
		if err != nil {
			return nil, err
		}
		rel, err := catalog.CleanRel(it.Path)
		if err != nil {
			return nil, err
		}
		wants = append(wants, want{src, rel})
	}
	var problems []string
	for n, w := range wants {
		db := sel.dbs[w.src.DB]
		if db == nil {
			var err error
			if db, err = idx.Open(w.src.DB); err != nil {
				sel.Close()
				return nil, err
			}
			sel.dbs[w.src.DB] = db
		}
		parts := strings.Split(w.rel, "/")
		b.progress().Update(fmt.Sprintf("Checking %d/%d: %s", n+1, len(wants), parts[len(parts)-1]), 0, 0)
		p, err := b.checkOne(db, w.src, w.rel)
		if err != nil {
			if r, ok := refusal.As(err); ok {
				problems = append(problems, r.Msg)
				continue
			}
			sel.Close()
			return nil, err
		}
		sel.Picks = append(sel.Picks, p)
	}
	if len(problems) > 0 {
		sel.Close()
		return nil, refusal.New("%s", strings.Join(problems, "\n"))
	}
	var keep []*Pick // drop items nested inside another selected item, and repeats
	for _, p := range sel.Picks {
		nested, dup := false, false
		for _, o := range sel.Picks {
			nested = nested || (o != p && o.Source == p.Source && o.IsDir && strings.HasPrefix(p.Rel, o.Rel+"/"))
		}
		for _, k := range keep {
			dup = dup || (k.Source == p.Source && k.Rel == p.Rel)
		}
		if !nested && !dup {
			keep = append(keep, p)
		}
	}
	sel.Picks = keep
	return sel, nil
}

func (b Box) checkOne(db *sql.DB, src *catalog.Source, rel string) (*Pick, error) {
	root := src.Root
	if !catalog.DirExists(root) {
		return nil, refusal.New("%s: the disk is not reachable from this computer (%s)", src.Raw, root)
	}
	full := filepath.Join(root, filepath.FromSlash(rel))
	if !catalog.Inside(root, full) {
		return nil, refusal.New("%s: outside the source", rel)
	}
	if err := b.guard(full); err != nil {
		return nil, refusal.New("%s: %v", rel, err)
	}
	lst, err := os.Lstat(full)
	if err != nil {
		return nil, refusal.New("%s: does not exist", rel)
	}
	if lst.Mode()&os.ModeSymlink != 0 {
		return nil, refusal.New("%s: is a symbolic link", rel)
	}
	p := &Pick{Source: src, Rel: rel, Full: full, DB: db}
	if lst.Mode().IsRegular() {
		rows, err := idx.FileAt(db, src.SID, rel)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			return nil, refusal.New("%s: not in the index (scan again)", rel)
		}
		if lst.Size() != rows[0].Size || lst.ModTime().Unix() != rows[0].Mtime {
			return nil, refusal.New("%s: modified since the last scan (scan again)", rel)
		}
		p.Files = rows
		return p, nil
	}
	if !lst.IsDir() {
		return nil, refusal.New("%s: is neither a file nor a folder", rel)
	}
	rows, err := idx.FilesUnder(db, src.SID, rel)
	if err != nil {
		return nil, err
	}
	type sig struct{ size, mtime int64 }
	known := map[string]sig{}
	for _, r := range rows {
		known[r.Path] = sig{r.Size, r.Mtime}
	}
	var bad error
	filepath.WalkDir(full, func(path string, d fs.DirEntry, err error) error {
		if err != nil || bad != nil {
			return nil
		}
		if d.IsDir() {
			if path != full && idx.SkipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if idx.SkipFile(d.Name()) {
			return nil
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil
		}
		r, _ := filepath.Rel(root, path)
		r = filepath.ToSlash(r)
		if k, ok := known[r]; !ok || k.size != info.Size() || k.mtime != info.ModTime().Unix() {
			bad = refusal.New("%s: the folder changed since the last scan (%s); scan again", rel, r)
		}
		return nil
	})
	if bad != nil {
		return nil, bad
	}
	p.IsDir, p.Files = true, rows
	return p, nil
}

// checkLastCopy refuses a move that would remove every remaining copy of a duplicated file,
// counting the copies on all plugged-in disks.
func checkLastCopy(liveDBs []string, picks []*Pick) error {
	moving := map[string]int{}
	for _, p := range picks {
		for _, f := range p.Files {
			if f.SHA.Valid {
				moving[f.SHA.String]++
			}
		}
	}
	if len(moving) == 0 {
		return nil
	}
	shas := make([]string, 0, len(moving))
	for k := range moving {
		shas = append(shas, k)
	}
	total := map[string]int{}
	for _, p := range liveDBs {
		db, err := idx.OpenRO(p)
		if err != nil {
			continue
		}
		for i := 0; i < len(shas); i += 500 {
			chunk := shas[i:min(i+500, len(shas))]
			args := make([]any, len(chunk))
			for j, c := range chunk {
				args[j] = c
			}
			rs, err := db.Query(`SELECT sha256, COUNT(*) FROM files WHERE sha256 IN (`+idx.Placeholders(len(chunk))+`) GROUP BY sha256`, args...)
			if err != nil {
				continue
			}
			for rs.Next() {
				var sha string
				var n int
				if rs.Scan(&sha, &n) == nil {
					total[sha] += n
				}
			}
			rs.Close()
		}
		db.Close()
	}
	var last []string
	for sha, n := range moving {
		if total[sha] >= 2 && total[sha] == n {
			last = append(last, sha)
		}
	}
	if len(last) == 0 {
		return nil
	}
	sort.Strings(last)
	example := ""
	for _, p := range liveDBs {
		db, err := idx.OpenRO(p)
		if err != nil {
			continue
		}
		var path string
		err = db.QueryRow(`SELECT path FROM files WHERE sha256=? LIMIT 1`, last[0]).Scan(&path)
		db.Close()
		if err == nil {
			example = path
			break
		}
	}
	return refusal.New("Refused: this would move ALL copies of %d duplicated files (e.g. %s). Keep at least one copy of each.", len(last), example)
}

var safeRe = regexp.MustCompile(`[^\p{L}\p{N}_ .\-]`)

func safeLabel(l string) string {
	l = strings.TrimSpace(safeRe.ReplaceAllString(l, "_"))
	if l == "" {
		return "source"
	}
	return l
}

// Moved is what LetGo did.
type Moved struct {
	Moved int    `json:"moved"`
	Files int    `json:"files"`
	Bytes int64  `json:"bytes"`
	Batch string `json:"batch"`
}

// LetGo moves items into the farewell box of their disks. Everything is checked before anything
// moves; then each item leaves the catalog as it goes.
func (b Box) LetGo(items []Item) (Moved, error) {
	var res Moved
	sel, err := b.Check(items, "moved")
	if err != nil {
		return res, err
	}
	defer sel.Close()
	if len(sel.Picks) == 0 {
		return res, refusal.New("nothing to move")
	}
	var live []string
	for _, d := range b.Catalog.Disks() {
		live = append(live, d.DB)
	}
	if err := checkLastCopy(live, sel.Picks); err != nil {
		return res, err
	}
	var total int64
	for _, p := range sel.Picks {
		total += int64(len(p.Files))
	}
	b.progress().Update("Moving to quarantine…", 0, total)
	res.Batch = time.Now().Format("2006-01-02_150405")
	backed := map[*catalog.Disk]bool{}
	for _, p := range sel.Picks {
		disk := p.Source.Disk
		if !backed[disk] {
			disk.Backup()
			backed[disk] = true
		}
		qrel := Dir + "/" + res.Batch + "/" + safeLabel(p.Source.Raw) + "/" + p.Rel
		qfull := filepath.Join(p.Source.Root, filepath.FromSlash(qrel))
		if err := os.MkdirAll(filepath.Dir(qfull), 0o755); err != nil {
			return res, err
		}
		var size int64
		ids := make([]int64, len(p.Files))
		for i, f := range p.Files {
			size += f.Size
			ids[i] = f.ID
		}
		if err := os.Rename(p.Full, qfull); err != nil {
			return res, refusal.New("%s: move failed (%v). Moved so far: %d items.", p.Rel, err, res.Moved)
		}
		tx, err := p.DB.Begin()
		if err != nil {
			return res, err
		}
		if err := idx.RecordMove(tx, idx.Move{Batch: res.Batch, SID: p.Source.SID, Path: p.Rel, QPath: qrel, IsDir: p.IsDir,
			Files: len(p.Files), Size: size, MovedAt: time.Now().Unix()}, ids); err != nil {
			tx.Rollback()
			return res, err
		}
		if err := tx.Commit(); err != nil {
			return res, err
		}
		b.progress().Advance(int64(len(p.Files)))
		disk.Log("quarantine", p.Source.Raw, p.Rel, len(p.Files), size)
		res.Moved++
		res.Files += len(p.Files)
		res.Bytes += size
	}
	return res, nil
}

// Waiting is one file or folder in the farewell box, as the page shows it.
type Waiting struct {
	ID      string `json:"id"` // <disk key>:<move id>
	Batch   string `json:"batch"`
	Source  string `json:"source"`
	Path    string `json:"path"`
	IsDir   int    `json:"is_dir"`
	Files   int    `json:"files"`
	Size    int64  `json:"size"`
	MovedAt int64  `json:"moved_at"`
}

// Contents lists what waits in the farewell boxes of the plugged-in disks, newest first.
func (b Box) Contents() []Waiting {
	out := []Waiting{}
	for _, d := range b.Catalog.Disks() {
		db, err := idx.OpenRO(d.DB)
		if err != nil {
			continue
		}
		moves, _ := idx.Moves(db)
		db.Close()
		for _, m := range moves {
			label := fmt.Sprint(m.SID)
			if x := b.Catalog.Source(d.DB, m.SID); x != nil {
				label = x.Label
			}
			isDir := 0
			if m.IsDir {
				isDir = 1
			}
			out = append(out, Waiting{fmt.Sprintf("%s:%d", d.Key, m.ID), m.Batch, label, m.Path, isDir, m.Files, m.Size, m.MovedAt})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].MovedAt > out[j].MovedAt })
	return out
}

// Restored is what Restore did.
type Restored struct {
	Restored string `json:"restored"`
	Note     string `json:"note"`
}

// Restore puts the item id (from Contents) back where it was, unless something took its place.
func (b Box) Restore(id string) (Restored, error) {
	var res Restored
	key, num, _ := strings.Cut(id, ":")
	disk := b.Catalog.Disk(key)
	if disk == nil {
		return res, refusal.New("the disk of this item is not plugged in")
	}
	db, err := idx.Open(disk.DB)
	if err != nil {
		return res, err
	}
	defer db.Close()
	m, err := idx.MoveByID(db, num)
	if err != nil {
		return res, refusal.New("item not found in quarantine")
	}
	src := b.Catalog.Source(disk.DB, m.SID)
	if src == nil {
		return res, refusal.New("source not found")
	}
	full, qfull := filepath.Join(src.Root, filepath.FromSlash(m.Path)), filepath.Join(src.Root, filepath.FromSlash(m.QPath))
	if !catalog.DirExists(src.Root) {
		return res, refusal.New("the disk is not reachable")
	}
	if err := b.guard(full); err != nil {
		return res, refusal.New("%s: %v", m.Path, err)
	}
	if _, err := os.Lstat(full); err == nil {
		return res, refusal.New("%s: a file or folder with that name already exists; not overwriting", m.Path)
	}
	if _, err := os.Lstat(qfull); err != nil {
		return res, refusal.New("%s: no longer in the quarantine folder", m.Path)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return res, err
	}
	if err := os.Rename(qfull, full); err != nil {
		return res, err
	}
	tx, err := db.Begin()
	if err != nil {
		return res, err
	}
	if err := idx.RecordRestore(tx, m); err != nil {
		tx.Rollback()
		return res, err
	}
	if err := tx.Commit(); err != nil {
		return res, err
	}
	disk.Log("restore", src.Label, m.Path, 0, m.Size)
	return Restored{m.Path, "Use Check now to restore the searchable text too."}, nil
}

// Emptied is what Empty did.
type Emptied struct {
	Deleted int   `json:"deleted"`
	Bytes   int64 `json:"bytes"`
}

// Empty deletes for good what waits in the farewell boxes of the plugged-in disks. confirm must
// be the words the user typed.
func (b Box) Empty(confirm string) (Emptied, error) {
	var res Emptied
	if !Confirmed(confirm) {
		return res, refusal.New("confirmation missing")
	}
	type box struct {
		d     *catalog.Disk
		db    *sql.DB
		moves []idx.Move
	}
	var boxes []box
	defer func() {
		for _, x := range boxes {
			x.db.Close()
		}
	}()
	var total int64
	for _, d := range b.Catalog.Disks() {
		db, err := idx.Open(d.DB)
		if err != nil {
			return res, err
		}
		moves, err := idx.Moves(db)
		boxes = append(boxes, box{d, db, moves})
		if err != nil {
			return res, err
		}
		for _, m := range moves {
			total += int64(m.Files)
		}
	}
	b.progress().Update("Deleting permanently…", 0, total)
	for _, x := range boxes {
		used := map[string]bool{}
		for _, m := range x.moves {
			src := b.Catalog.Source(x.d.DB, m.SID)
			if src == nil || !catalog.DirExists(src.Root) {
				continue
			}
			b.rmTree(filepath.Join(src.Root, filepath.FromSlash(m.QPath)))
			idx.RecordDeleted(x.db, m.ID)
			x.d.Log("delete", src.Label, m.QPath, 0, m.Size)
			res.Bytes += m.Size
			res.Deleted++
			used[src.Root] = true
		}
		for root := range used { // remove emptied batch folders, keep anything unexpected
			base := filepath.Join(root, filepath.FromSlash(Dir))
			var dirs []string
			filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
				if err == nil && d.IsDir() {
					dirs = append(dirs, p)
				}
				return nil
			})
			for i := len(dirs) - 1; i >= 0; i-- {
				os.Remove(dirs[i])
			}
		}
	}
	return res, nil
}

// rmTree deletes a file or folder, counting files for the progress bar.
func (b Box) rmTree(path string) {
	st, err := os.Lstat(path)
	if err != nil {
		return
	}
	if !st.IsDir() {
		if os.Remove(path) == nil {
			b.progress().Advance(1)
		}
		return
	}
	var dirs []string
	filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			dirs = append(dirs, p)
			return nil
		}
		if os.Remove(p) == nil {
			b.progress().Advance(1)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		os.Remove(dirs[i])
	}
}
