package server

import (
	"database/sql"
	"encoding/csv"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/sim186/disk-sidecar/internal/assets"
	"github.com/sim186/disk-sidecar/internal/idx"
	"github.com/sim186/disk-sidecar/internal/scan"
)

const qdir = ".sidecar/_quarantine"

var safeRe = regexp.MustCompile(`[^\p{L}\p{N}_ .\-]`)

func safeLabel(l string) string {
	l = strings.TrimSpace(safeRe.ReplaceAllString(l, "_"))
	if l == "" {
		return "source"
	}
	return l
}

// cleanRel validates a path sent by the page: relative, no "..", never inside .sidecar.
func cleanRel(rel string) (string, error) {
	rel = strings.Trim(strings.ReplaceAll(rel, "\\", "/"), "/")
	parts := strings.Split(rel, "/")
	bad := rel == ""
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || p == ".sidecar" {
			bad = true
		}
	}
	if !bad && strings.Contains(parts[0], ":") {
		bad = true
	}
	if bad {
		return "", refuse("invalid path: %q", rel)
	}
	return rel, nil
}

func realpath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	a, _ := filepath.Abs(p)
	return a
}

// inside reports whether full is strictly inside root, after resolving links.
func inside(root, full string) bool {
	r, f := realpath(root), realpath(full)
	rel, err := filepath.Rel(r, f)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (s *Server) logAction(d *Disk, action, source, path string, nfiles int, nbytes int64) {
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

// ---------------------------------------------------------------- marks

type markItem struct {
	Source string  `json:"source"`
	Path   string  `json:"path"`
	Mark   *string `json:"mark"`
	Note   *string `json:"note"`
}

func (s *Server) doMarks(items []markItem) (any, error) {
	srcs, _ := s.listSources()
	by := map[string]*Src{}
	for _, x := range srcs {
		by[x.Label] = x
	}
	type mk struct {
		sid  int64
		rel  string
		mark *string
		note *string
	}
	groups := map[string][]mk{}
	for _, it := range items {
		src := by[it.Source]
		if src == nil {
			return nil, refuse("unknown source: %s", it.Source)
		}
		if src.Kind != "disk" && src.Kind != "phone" {
			return nil, refuse("%s is %s: marks must be set from its own computer", src.Label, readOnlyWhat(src.Kind))
		}
		rel, err := cleanRel(it.Path)
		if err != nil {
			return nil, err
		}
		if it.Mark != nil && *it.Mark != "keep" && *it.Mark != "delete" && *it.Mark != "review" {
			return nil, refuse("invalid mark: %q", *it.Mark)
		}
		groups[src.DB] = append(groups[src.DB], mk{src.SID, rel, it.Mark, it.Note})
	}
	for dbPath, rows := range groups {
		db, err := idx.Open(dbPath)
		if err != nil {
			return nil, err
		}
		tx, err := db.Begin()
		if err != nil {
			db.Close()
			return nil, err
		}
		for _, r := range rows {
			if r.mark == nil {
				_, err = tx.Exec(`DELETE FROM marks WHERE source_id=? AND path=?`, r.sid, r.rel)
			} else {
				_, err = tx.Exec(`INSERT INTO marks(source_id, path, mark, note) VALUES (?,?,?,?)
					ON CONFLICT(source_id, path) DO UPDATE SET mark=excluded.mark, note=excluded.note`, r.sid, r.rel, *r.mark, r.note)
			}
			if err != nil {
				tx.Rollback()
				db.Close()
				return nil, err
			}
		}
		err = tx.Commit()
		db.Close()
		if err != nil {
			return nil, err
		}
	}
	s.gen.Add(1)
	return map[string]any{"saved": len(items)}, nil
}

// ---------------------------------------------------------------- quarantine

type moveRow struct {
	id, size, mtime int64
	path            string
	sha             sql.NullString
}

type move struct {
	label, root, rel, full string
	sid                    int64
	isDir                  bool
	rows                   []moveRow
	db                     *sql.DB
	disk                   *Disk
}

// plan validates the items of ONE disk index (raw labels) and returns what would be moved.
func plan(db *sql.DB, items []struct{ Source, Path string }, job *Job, diskRoot string, guard func(string) error) ([]*move, error) {
	type sr struct {
		sid  int64
		root string
	}
	srcs := map[string]sr{}
	rows, err := db.Query(`SELECT id, label, COALESCE(root,'') FROM sources`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var label, root string
		if rows.Scan(&id, &label, &root) == nil {
			srcs[label] = sr{id, root}
		}
	}
	rows.Close()
	for l, x := range srcs {
		srcs[l] = sr{x.sid, idx.ResolveRoot(db, x.sid, x.root, diskRoot)}
	}
	var problems []string
	var moves []*move
	for n, it := range items {
		if job != nil {
			parts := strings.Split(it.Path, "/")
			job.Update(fmt.Sprintf("Checking %d/%d: %s", n+1, len(items), parts[len(parts)-1]), 0, 0)
		}
		m, err := planOne(db, srcs[it.Source].sid, srcs[it.Source].root, it.Source, it.Path, guard)
		if err != nil {
			var r *Refused
			if ok := asRefused(err, &r); ok {
				problems = append(problems, r.Msg)
				continue
			}
			return nil, err
		}
		moves = append(moves, m)
	}
	if len(problems) > 0 {
		return nil, refuse("%s", strings.Join(problems, "\n"))
	}
	var keep []*move // drop items nested inside another selected item
	for _, m := range moves {
		nested := false
		for _, o := range moves {
			if o != m && o.sid == m.sid && o.isDir && strings.HasPrefix(m.rel, o.rel+"/") {
				nested = true
			}
		}
		dup := false
		for _, k := range keep {
			dup = dup || (k.sid == m.sid && k.rel == m.rel)
		}
		if !nested && !dup {
			keep = append(keep, m)
		}
	}
	return keep, nil
}

func asRefused(err error, out **Refused) bool {
	r, ok := err.(*Refused)
	if ok {
		*out = r
	}
	return ok
}

func planOne(db *sql.DB, sid int64, root, label, rel string, guard func(string) error) (*move, error) {
	if !dirExists(root) {
		return nil, refuse("%s: the disk is not reachable from this computer (%s)", label, root)
	}
	full := filepath.Join(root, filepath.FromSlash(rel))
	if !inside(root, full) {
		return nil, refuse("%s: outside the source", rel)
	}
	if err := guard(full); err != nil {
		return nil, refuse("%s: %v", rel, err)
	}
	lst, err := os.Lstat(full)
	if err != nil {
		return nil, refuse("%s: does not exist", rel)
	}
	if lst.Mode()&os.ModeSymlink != 0 {
		return nil, refuse("%s: is a symbolic link", rel)
	}
	m := &move{label: label, root: root, rel: rel, full: full, sid: sid}
	load := func(q string, args ...any) ([]moveRow, error) {
		rs, err := db.Query(q, args...)
		if err != nil {
			return nil, err
		}
		defer rs.Close()
		var out []moveRow
		for rs.Next() {
			var r moveRow
			if rs.Scan(&r.id, &r.path, &r.size, &r.mtime, &r.sha) == nil {
				out = append(out, r)
			}
		}
		return out, nil
	}
	if lst.Mode().IsRegular() {
		rows, err := load(`SELECT id, path, size, mtime, sha256 FROM files WHERE source_id=? AND path=?`, sid, rel)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			return nil, refuse("%s: not in the index (scan again)", rel)
		}
		if lst.Size() != rows[0].size || lst.ModTime().Unix() != rows[0].mtime {
			return nil, refuse("%s: modified since the last scan (scan again)", rel)
		}
		m.rows = rows
		return m, nil
	}
	if !lst.IsDir() {
		return nil, refuse("%s: is neither a file nor a folder", rel)
	}
	pre := rel + "/"
	rows, err := load(`SELECT id, path, size, mtime, sha256 FROM files WHERE source_id=? AND substr(path,1,?)=?`, sid, len(pre), pre)
	if err != nil {
		return nil, err
	}
	type sig struct{ size, mtime int64 }
	known := map[string]sig{}
	for _, r := range rows {
		known[r.path] = sig{r.size, r.mtime}
	}
	var bad error
	filepath.WalkDir(full, func(p string, d fs.DirEntry, err error) error {
		if err != nil || bad != nil {
			return nil
		}
		if d.IsDir() {
			if p != full && idx.SkipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if idx.SkipFile(d.Name()) {
			return nil
		}
		info, err := os.Stat(p)
		if err != nil {
			return nil
		}
		r, _ := filepath.Rel(root, p)
		r = filepath.ToSlash(r)
		if k, ok := known[r]; !ok || k.size != info.Size() || k.mtime != info.ModTime().Unix() {
			bad = refuse("%s: the folder changed since the last scan (%s); scan again", rel, r)
		}
		return nil
	})
	if bad != nil {
		return nil, bad
	}
	m.isDir, m.rows = true, rows
	return m, nil
}

// checkLastCopy refuses a move that would remove every remaining copy of a duplicated file,
// counting the copies on all plugged-in disks.
func checkLastCopy(liveDBs []string, all []*move) error {
	moving := map[string]int{}
	for _, m := range all {
		for _, r := range m.rows {
			if r.sha.Valid {
				moving[r.sha.String]++
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
			rs, err := db.Query(`SELECT sha256, COUNT(*) FROM files WHERE sha256 IN (`+placeholders(len(chunk))+`) GROUP BY sha256`, args...)
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
	return refuse("Refused: this would move ALL copies of %d duplicated files (e.g. %s). Keep at least one copy of each.", len(last), example)
}

type qItem struct {
	Source string `json:"source"`
	Path   string `json:"path"`
}

// planItems validates items that live on any number of disks and returns what they hold. The
// caller must call done to close the index handles; handles are by index path.
func (s *Server) planItems(items []qItem, job *Job, verb string) (plans []*move, handles map[string]*sql.DB, disks []*Disk, done func(), err error) {
	srcs, disks := s.listSources()
	by := map[string]*Src{}
	for _, x := range srcs {
		by[x.Label] = x
	}
	type group struct {
		disk  *Disk
		items []struct{ Source, Path string }
	}
	groups := map[string]*group{}
	var order []string
	for _, it := range items {
		src := by[it.Source]
		if src == nil {
			return nil, nil, nil, nil, refuse("unknown source: %s", it.Source)
		}
		if src.Kind != "disk" {
			return nil, nil, nil, nil, refuse("%s is %s: its files cannot be %s", src.Label, readOnlyWhat(src.Kind), verb)
		}
		rel, err := cleanRel(it.Path)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		g := groups[src.DB]
		if g == nil {
			g = &group{disk: src.Disk}
			groups[src.DB] = g
			order = append(order, src.DB)
		}
		g.items = append(g.items, struct{ Source, Path string }{src.Raw, rel})
	}
	handles = map[string]*sql.DB{}
	done = func() {
		for _, h := range handles {
			h.Close()
		}
	}
	for _, dbPath := range order {
		g := groups[dbPath]
		db, err := idx.Open(dbPath)
		if err != nil {
			done()
			return nil, nil, nil, nil, err
		}
		handles[dbPath] = db
		ms, err := plan(db, g.items, job, g.disk.Root, s.guardPath)
		if err != nil {
			done()
			return nil, nil, nil, nil, err
		}
		for _, m := range ms {
			m.db, m.disk = db, g.disk
			plans = append(plans, m)
		}
	}
	return plans, handles, disks, done, nil
}

func (s *Server) doQuarantine(items []qItem, job *Job) (any, error) {
	plans, _, disks, closeAll, err := s.planItems(items, job, "moved")
	if err != nil {
		return nil, err
	}
	defer closeAll()
	if len(plans) == 0 {
		return nil, refuse("nothing to move")
	}
	var live []string
	for _, d := range disks {
		live = append(live, d.DB)
	}
	if err := checkLastCopy(live, plans); err != nil {
		return nil, err
	}
	var total int64
	for _, m := range plans {
		total += int64(len(m.rows))
	}
	if job != nil {
		job.Update("Moving to quarantine…", 0, total)
	}
	batch := time.Now().Format("2006-01-02_150405")
	backed := map[string]bool{}
	var done int
	var nfiles int
	var nbytes int64
	for _, m := range plans {
		if !backed[m.disk.DB] {
			copyFile(m.disk.DB, m.disk.DB+".bak")
			backed[m.disk.DB] = true
		}
		qrel := qdir + "/" + batch + "/" + safeLabel(m.label) + "/" + m.rel
		qfull := filepath.Join(m.root, filepath.FromSlash(qrel))
		if err := os.MkdirAll(filepath.Dir(qfull), 0o755); err != nil {
			return nil, err
		}
		var size int64
		for _, r := range m.rows {
			size += r.size
		}
		if err := os.Rename(m.full, qfull); err != nil {
			return nil, refuse("%s: move failed (%v). Moved so far: %d items.", m.rel, err, done)
		}
		tx, err := m.db.Begin()
		if err != nil {
			return nil, err
		}
		isDir := 0
		if m.isDir {
			isDir = 1
		}
		res, err := tx.Exec(`INSERT INTO quarantine_moves(batch, source_id, path, qpath, is_dir, nfiles, size, moved_at) VALUES (?,?,?,?,?,?,?,?)`,
			batch, m.sid, m.rel, qrel, isDir, len(m.rows), size, time.Now().Unix())
		if err != nil {
			tx.Rollback()
			return nil, err
		}
		mid, _ := res.LastInsertId()
		for i := 0; i < len(m.rows); i += 500 {
			chunk := m.rows[i:min(i+500, len(m.rows))]
			ids := make([]any, len(chunk))
			for j, r := range chunk {
				ids[j] = r.id
			}
			ph := placeholders(len(chunk))
			for _, q := range []string{
				`INSERT INTO quarantine_files SELECT ?, path, name, ext, size, mtime, kind, top, sha256 FROM files WHERE id IN (` + ph + `)`,
				`DELETE FROM file_text WHERE rowid IN (` + ph + `)`,
				`DELETE FROM files WHERE id IN (` + ph + `)`,
			} {
				args := ids
				if strings.HasPrefix(q, "INSERT") {
					args = append([]any{mid}, ids...)
				}
				if _, err := tx.Exec(q, args...); err != nil {
					tx.Rollback()
					return nil, err
				}
			}
			if job != nil {
				job.Advance(int64(len(chunk)))
			}
		}
		if _, err := tx.Exec(`DELETE FROM marks WHERE source_id=? AND (path=? OR substr(path,1,?)=?)`, m.sid, m.rel, len(m.rel)+1, m.rel+"/"); err != nil {
			tx.Rollback()
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		s.logAction(m.disk, "quarantine", m.label, m.rel, len(m.rows), size)
		done++
		nfiles += len(m.rows)
		nbytes += size
	}
	return map[string]any{"moved": done, "files": nfiles, "bytes": nbytes, "batch": batch}, nil
}

func copyFile(src, dst string) error {
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

type qMove struct {
	ID      string `json:"id"`
	Batch   string `json:"batch"`
	Source  string `json:"source"`
	Path    string `json:"path"`
	IsDir   int    `json:"is_dir"`
	Files   int    `json:"files"`
	Size    int64  `json:"size"`
	MovedAt int64  `json:"moved_at"`
}

func (s *Server) quarantineState(srcs []*Src, disks []*Disk) []qMove {
	moves := []qMove{}
	for _, d := range disks {
		db, err := idx.OpenRO(d.DB)
		if err != nil {
			continue
		}
		labels := map[int64]string{}
		for _, x := range srcs {
			if x.DB == d.DB {
				labels[x.SID] = x.Label
			}
		}
		rows, err := db.Query(`SELECT id, batch, source_id, path, is_dir, nfiles, size, moved_at FROM quarantine_moves WHERE state='moved' ORDER BY id DESC`)
		if err == nil {
			for rows.Next() {
				var id, sid, size, at int64
				var batch, path string
				var isDir, n int
				if rows.Scan(&id, &batch, &sid, &path, &isDir, &n, &size, &at) == nil {
					l := labels[sid]
					if l == "" {
						l = fmt.Sprint(sid)
					}
					moves = append(moves, qMove{fmt.Sprintf("%s:%d", d.Key, id), batch, l, path, isDir, n, size, at})
				}
			}
			rows.Close()
		}
		db.Close()
	}
	sort.SliceStable(moves, func(i, j int) bool { return moves[i].MovedAt > moves[j].MovedAt })
	return moves
}

func (s *Server) doRestore(moveID string) (any, error) {
	key, num, _ := strings.Cut(moveID, ":")
	srcs, disks := s.listSources()
	var disk *Disk
	for _, d := range disks {
		if d.Key == key {
			disk = d
		}
	}
	if disk == nil {
		return nil, refuse("the disk of this item is not plugged in")
	}
	db, err := idx.Open(disk.DB)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var id, sid, size int64
	var rel, qrel string
	if err := db.QueryRow(`SELECT id, source_id, path, qpath, size FROM quarantine_moves WHERE id=? AND state='moved'`, num).
		Scan(&id, &sid, &rel, &qrel, &size); err != nil {
		return nil, refuse("item not found in quarantine")
	}
	var src *Src
	for _, x := range srcs {
		if x.DB == disk.DB && x.SID == sid {
			src = x
		}
	}
	if src == nil {
		return nil, refuse("source not found")
	}
	full, qfull := filepath.Join(src.Root, filepath.FromSlash(rel)), filepath.Join(src.Root, filepath.FromSlash(qrel))
	if !dirExists(src.Root) {
		return nil, refuse("the disk is not reachable")
	}
	if err := s.guardPath(full); err != nil {
		return nil, refuse("%s: %v", rel, err)
	}
	if _, err := os.Lstat(full); err == nil {
		return nil, refuse("%s: a file or folder with that name already exists; not overwriting", rel)
	}
	if _, err := os.Lstat(qfull); err != nil {
		return nil, refuse("%s: no longer in the quarantine folder", rel)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return nil, err
	}
	if err := os.Rename(qfull, full); err != nil {
		return nil, err
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	for _, q := range []string{
		`INSERT OR IGNORE INTO files(source_id, path, name, ext, size, mtime, kind, top, sha256, seen) SELECT ?, path, name, ext, size, mtime, kind, top, sha256, 1 FROM quarantine_files WHERE move_id=?`,
	} {
		if _, err := tx.Exec(q, sid, id); err != nil {
			tx.Rollback()
			return nil, err
		}
	}
	tx.Exec(`DELETE FROM quarantine_files WHERE move_id=?`, id)
	tx.Exec(`UPDATE quarantine_moves SET state='restored' WHERE id=?`, id)
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.logAction(disk, "restore", src.Label, rel, 0, size)
	s.gen.Add(1)
	return map[string]any{"restored": rel, "note": "Use Check now to restore the searchable text too."}, nil
}

// rmTree deletes a file or folder, counting files for the progress bar.
func rmTree(path string, job *Job) error {
	st, err := os.Lstat(path)
	if err != nil {
		return nil
	}
	if !st.IsDir() {
		err := os.Remove(path)
		if err == nil && job != nil {
			job.Advance(1)
		}
		return err
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
		if e := os.Remove(p); e == nil && job != nil {
			job.Advance(1)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		os.Remove(dirs[i])
	}
	return nil
}

func (s *Server) doEmpty(confirm string, job *Job) (any, error) {
	if confirm != "DELETE" {
		return nil, refuse("confirmation missing")
	}
	srcs, disks := s.listSources()
	type dbh struct {
		d  *Disk
		db *sql.DB
	}
	var hs []dbh
	defer func() {
		for _, h := range hs {
			h.db.Close()
		}
	}()
	var total int64
	for _, d := range disks {
		db, err := idx.Open(d.DB)
		if err != nil {
			return nil, err
		}
		hs = append(hs, dbh{d, db})
		var n int64
		db.QueryRow(`SELECT COALESCE(SUM(nfiles),0) FROM quarantine_moves WHERE state='moved'`).Scan(&n)
		total += n
	}
	if job != nil {
		job.Update("Deleting permanently…", 0, total)
	}
	var freed int64
	count := 0
	for _, h := range hs {
		roots := map[int64]*Src{}
		for _, x := range srcs {
			if x.DB == h.d.DB {
				roots[x.SID] = x
			}
		}
		rows, err := h.db.Query(`SELECT id, qpath, size, source_id FROM quarantine_moves WHERE state='moved'`)
		if err != nil {
			return nil, err
		}
		type mv struct {
			id, size, sid int64
			qpath         string
		}
		var list []mv
		for rows.Next() {
			var m mv
			if rows.Scan(&m.id, &m.qpath, &m.size, &m.sid) == nil {
				list = append(list, m)
			}
		}
		rows.Close()
		used := map[string]bool{}
		for _, m := range list {
			src := roots[m.sid]
			if src == nil || !dirExists(src.Root) {
				continue
			}
			rmTree(filepath.Join(src.Root, filepath.FromSlash(m.qpath)), job)
			h.db.Exec(`DELETE FROM quarantine_files WHERE move_id=?`, m.id)
			h.db.Exec(`UPDATE quarantine_moves SET state='deleted' WHERE id=?`, m.id)
			s.logAction(h.d, "delete", src.Label, m.qpath, 0, m.size)
			freed += m.size
			count++
			used[src.Root] = true
		}
		for root := range used { // remove emptied batch folders, keep anything unexpected
			base := filepath.Join(root, filepath.FromSlash(qdir))
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
	return map[string]any{"deleted": count, "bytes": freed}, nil
}

// ---------------------------------------------------------------- verification of changes made by hand

func (s *Server) doVerify(job *Job) (any, error) {
	srcs, _ := s.listSources()
	host, _ := os.Hostname()
	totals := map[string]any{"new": 0, "changed": 0, "removed": 0, "errors": 0, "disks": 0, "secs": 0.0,
		"at": time.Now().Unix(), "details": []map[string]any{}}
	var nNew, nCh, nRm, nErr, nDisks int
	var secs float64
	details := []map[string]any{}
	for _, src := range srcs {
		if src.Kind != "disk" || !dirExists(src.Root) {
			continue
		}
		hint := int64(0)
		if db, err := idx.OpenRO(src.DB); err == nil {
			db.QueryRow(`SELECT COUNT(*) FROM files WHERE source_id=?`, src.SID).Scan(&hint)
			db.Close()
		}
		if job != nil {
			job.Update("Checking "+src.Label+"…", 0, hint)
		}
		st, err := scan.Run(scan.Options{Root: src.Root, DB: src.DB, Label: src.Raw, Host: host, HashMin: -1, Text: true,
			Progress: func(n int) {
				if job != nil {
					job.SetDone(int64(n))
				}
			}})
		if err != nil {
			return nil, err
		}
		if db, err := idx.Open(src.DB); err == nil {
			db.Exec(`INSERT INTO scan_log VALUES (?,?,?,?,?,?,?,?)`, time.Now().Unix(), src.SID, "verify", st.New, st.Changed, st.Removed, st.Errors, st.Secs)
			db.Close()
		}
		nNew, nCh, nRm, nErr, nDisks, secs = nNew+st.New, nCh+st.Changed, nRm+st.Removed, nErr+st.Errors, nDisks+1, secs+st.Secs
		if st.New+st.Changed+st.Removed > 0 {
			details = append(details, map[string]any{"source": src.Label, "new": st.New, "changed": st.Changed, "removed": st.Removed})
		}
	}
	totals["new"], totals["changed"], totals["removed"], totals["errors"] = nNew, nCh, nRm, nErr
	totals["disks"], totals["secs"], totals["details"] = nDisks, secs, details
	s.lastVerify.Store(&totals)
	return totals, nil
}

// ---------------------------------------------------------------- state for the page

func (s *Server) state() map[string]any {
	srcs, disks := s.listSources()
	var warnings []string
	ids := map[string]string{}
	for _, d := range disks {
		id := diskIDOf(d.DB)
		if prev, ok := ids[id]; ok && id != "" {
			warnings = append(warnings, fmt.Sprintf("Two disks have the same ID (%s and %s): one is a clone of the other.", prev, d.Root))
		}
		ids[id] = d.Root
	}
	type sv struct {
		Label     string `json:"label"`
		Kind      string `json:"kind"`
		Reachable bool   `json:"reachable"`
		ScannedAt int64  `json:"scanned_at"`
	}
	out := []sv{}
	for _, x := range srcs {
		out = append(out, sv{x.Label, x.Kind, x.Kind == "disk" && dirExists(x.Root), x.ScannedAt})
	}
	var startup any
	if s.startupJob != "" {
		startup = s.startupJob
	}
	var verify any
	if v := s.lastVerify.Load(); v != nil {
		verify = *v
	}
	if warnings == nil {
		warnings = []string{}
	}
	return map[string]any{"actions": true, "version": assets.Version, "quarantine": s.quarantineState(srcs, disks),
		"sources": out, "startup_job": startup, "verify": verify, "warnings": warnings}
}

// ---------------------------------------------------------------- search inside files

type hit struct {
	Source string `json:"source"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Kind   string `json:"kind"`
	How    string `json:"how"`
}

// searchText looks for words inside the extracted text of every disk and snapshot. The page does
// not carry that text (it can be gigabytes); it asks here.
func (s *Server) searchText(q map[string][]string) []hit {
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	var terms []string
	for _, t := range strings.Fields(get("q")) {
		terms = append(terms, `"`+strings.ReplaceAll(t, `"`, `""`)+`"*`)
	}
	out := []hit{}
	if len(terms) == 0 {
		return out
	}
	srcs, _ := s.listSources()
	byDB := map[string][]*Src{}
	var order []string
	for _, x := range srcs {
		if _, ok := byDB[x.DB]; !ok {
			order = append(order, x.DB)
		}
		byDB[x.DB] = append(byDB[x.DB], x)
	}
	for _, dbPath := range order {
		db, err := idx.OpenRO(dbPath)
		if err != nil {
			continue
		}
		for _, x := range byDB[dbPath] {
			if want := get("source"); want != "" && want != x.Label {
				continue
			}
			sqlq := `SELECT f.path, f.size, f.kind FROM file_text t JOIN files f ON f.id = t.rowid WHERE file_text MATCH ? AND f.source_id = ?`
			args := []any{strings.Join(terms, " "), x.SID}
			if k := get("kind"); k != "" && k != "all" {
				sqlq += ` AND f.kind = ?`
				args = append(args, k)
			}
			rows, err := db.Query(sqlq+` LIMIT 150`, args...)
			if err != nil {
				continue
			}
			for rows.Next() {
				h := hit{Source: x.Label, How: "content"}
				if rows.Scan(&h.Path, &h.Size, &h.Kind) == nil {
					out = append(out, h)
				}
			}
			rows.Close()
		}
		db.Close()
	}
	if len(out) > 150 {
		out = out[:150]
	}
	return out
}
