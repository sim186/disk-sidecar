package idx

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Everything that changes an index goes through this file: what a file row holds, the farewell
// box, marks and logs. Reading stays plain SQL: the layout is documented for scripts and LLMs.

// Querier is a *sql.DB or a *sql.Tx.
type Querier interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// NameParts is what the index keeps about a path besides the path itself: the file name, the
// lower-case extension (like Python's splitext, ".bashrc" has none) and the top folder.
func NameParts(rel string) (name, ext, top string) {
	name = rel[strings.LastIndex(rel, "/")+1:]
	if i := strings.LastIndex(name, "."); i > 0 {
		ext = strings.ToLower(name[i+1:])
	}
	if i := strings.Index(rel, "/"); i >= 0 {
		top = rel[:i]
	}
	return name, ext, top
}

const upsertFile = `INSERT INTO files(source_id,path,name,ext,size,mtime,kind,top,sha256,seen) VALUES (?,?,?,?,?,?,?,?,?,1)
	ON CONFLICT(source_id,path) DO UPDATE SET size=excluded.size, mtime=excluded.mtime, kind=excluded.kind,
	top=excluded.top, sha256=excluded.sha256, seen=1 RETURNING id`

// FileWriter records the files of one source inside one transaction. A scan writes millions of
// rows, so the statement is prepared once; make a new writer after each commit.
type FileWriter struct {
	tx  *sql.Tx
	st  *sql.Stmt
	sid int64
}

func NewFileWriter(tx *sql.Tx, sid int64) (*FileWriter, error) {
	st, err := tx.Prepare(upsertFile)
	if err != nil {
		return nil, err
	}
	return &FileWriter{tx, st, sid}, nil
}

// Put adds or updates the file at rel and returns its id.
func (w *FileWriter) Put(rel string, size, mtime int64, sha sql.NullString) (int64, error) {
	name, ext, top := NameParts(rel)
	var id int64
	err := w.st.QueryRow(w.sid, rel, name, ext, size, mtime, KindOf(ext), top, sha).Scan(&id)
	return id, err
}

// PutText replaces the searchable text of file id.
func (w *FileWriter) PutText(id int64, rel, body string) error {
	if _, err := w.tx.Exec(`DELETE FROM file_text WHERE rowid=?`, id); err != nil {
		return err
	}
	_, err := w.tx.Exec(`INSERT INTO file_text(rowid, path, body) VALUES (?,?,?)`, id, rel, body)
	return err
}

func (w *FileWriter) Close() error { return w.st.Close() }

// SetHash records the SHA-256 of file id, read just now.
func SetHash(q Querier, id int64, sha string) error {
	_, err := q.Exec(`UPDATE files SET sha256=? WHERE id=?`, sha, id)
	return err
}

// EnsureSource returns the id of the source called label, creating it, and records where it was
// read from.
func EnsureSource(q Querier, label, root, host string) (int64, error) {
	if _, err := q.Exec(`INSERT INTO sources(label, root, host) VALUES (?,?,?)
		ON CONFLICT(label) DO UPDATE SET root=excluded.root, host=excluded.host`, label, root, host); err != nil {
		return 0, err
	}
	var sid int64
	err := q.QueryRow(`SELECT id FROM sources WHERE label=?`, label).Scan(&sid)
	return sid, err
}

// MarkScanned records that source sid was read completely just now.
func MarkScanned(q Querier, sid int64) error {
	_, err := q.Exec(`UPDATE sources SET scanned_at=? WHERE id=?`, time.Now().Unix(), sid)
	return err
}

// RemovePath forgets the file at rel of source sid, and its text.
func RemovePath(q Querier, sid int64, rel string) error {
	if _, err := q.Exec(`DELETE FROM file_text WHERE rowid IN (SELECT id FROM files WHERE source_id=? AND path=?)`, sid, rel); err != nil {
		return err
	}
	_, err := q.Exec(`DELETE FROM files WHERE source_id=? AND path=?`, sid, rel)
	return err
}

// RemoveFile forgets file id, and its text.
func RemoveFile(q Querier, id int64) error {
	if _, err := q.Exec(`DELETE FROM file_text WHERE rowid=?`, id); err != nil {
		return err
	}
	_, err := q.Exec(`DELETE FROM files WHERE id=?`, id)
	return err
}

var likeEsc = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// LikePrefix is a LIKE pattern (with ESCAPE '\') for everything below folder rel.
func LikePrefix(rel string) string { return likeEsc.Replace(rel) + "/%" }

// DropTextUnder forgets the text of the files of source sid inside any folder called dir.
func DropTextUnder(q Querier, sid int64, dir string) error {
	_, err := q.Exec(`DELETE FROM file_text WHERE rowid IN (SELECT id FROM files WHERE source_id=? AND (path LIKE ? ESCAPE '\' OR path LIKE ? ESCAPE '\'))`,
		sid, LikePrefix(dir), "%/"+LikePrefix(dir))
	return err
}

// ClearSource forgets every file of source sid, keeping the source.
func ClearSource(q Querier, sid int64) error {
	if _, err := q.Exec(`DELETE FROM file_text WHERE rowid IN (SELECT id FROM files WHERE source_id=?)`, sid); err != nil {
		return err
	}
	_, err := q.Exec(`DELETE FROM files WHERE source_id=?`, sid)
	return err
}

// ReplaceSource makes an empty source called label, forgetting the files of one that exists.
func ReplaceSource(q Querier, label, root, host string, scannedAt int64) (sid int64, replaced bool, err error) {
	if q.QueryRow(`SELECT id FROM sources WHERE label=?`, label).Scan(&sid) == nil {
		if err := ClearSource(q, sid); err != nil {
			return 0, false, err
		}
		_, err := q.Exec(`UPDATE sources SET root=?, host=?, scanned_at=? WHERE id=?`, root, host, scannedAt, sid)
		return sid, true, err
	}
	res, err := q.Exec(`INSERT INTO sources(label, root, host, scanned_at) VALUES (?,?,?,?)`, label, root, host, scannedAt)
	if err != nil {
		return 0, false, err
	}
	sid, err = res.LastInsertId()
	return sid, false, err
}

// CopySource copies the files of source from in the index attached as alias into source to.
// With text, their searchable text comes too; it returns how many texts were copied.
func CopySource(q Querier, alias string, from, to int64, text bool) (int64, error) {
	if _, err := q.Exec(`INSERT INTO files(source_id, path, name, ext, size, mtime, kind, top, sha256, seen)
		SELECT ?, path, name, ext, size, mtime, kind, top, sha256, 1 FROM `+alias+`.files WHERE source_id=?`, to, from); err != nil {
		return 0, err
	}
	if !text {
		return 0, nil
	}
	res, err := q.Exec(`INSERT INTO file_text(rowid, path, body)
		SELECT m.id, t.path, t.body FROM `+alias+`.file_text t JOIN `+alias+`.files sf ON sf.id = t.rowid
		JOIN files m ON m.source_id = ? AND m.path = sf.path WHERE sf.source_id = ?`, to, from)
	if err != nil {
		return 0, nil // the files are there; text is a bonus
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// SetMeta records a value about the whole index.
func SetMeta(q Querier, key, value string) error {
	_, err := q.Exec(`INSERT OR REPLACE INTO meta VALUES (?, ?)`, key, value)
	return err
}

// SetMark puts the file or folder rel of source sid in a pile, or out of every pile if mark is nil.
func SetMark(q Querier, sid int64, rel string, mark, note *string) error {
	if mark == nil {
		_, err := q.Exec(`DELETE FROM marks WHERE source_id=? AND path=?`, sid, rel)
		return err
	}
	if *mark != "keep" && *mark != "delete" && *mark != "review" {
		return fmt.Errorf("invalid mark: %q", *mark)
	}
	_, err := q.Exec(`INSERT INTO marks(source_id, path, mark, note) VALUES (?,?,?,?)
		ON CONFLICT(source_id, path) DO UPDATE SET mark=excluded.mark, note=excluded.note`, sid, rel, *mark, note)
	return err
}

// LogScan records the outcome of reading source sid in the scan log.
func LogScan(q Querier, sid int64, kind string, nNew, changed, removed, errors int, secs float64) error {
	_, err := q.Exec(`INSERT INTO scan_log VALUES (?,?,?,?,?,?,?,?)`, time.Now().Unix(), sid, kind, nNew, changed, removed, errors, secs)
	return err
}

// ---------------------------------------------------------------- the farewell box

// Move is one file or folder in the farewell box (the quarantine).
type Move struct {
	ID, SID int64
	Batch   string
	Path    string // where it was, relative to the source
	QPath   string // where it is now, relative to the source
	IsDir   bool
	Files   int
	Size    int64
	MovedAt int64
}

// RecordMove notes that a file or folder was moved into the farewell box: the files it held
// (ids) leave the catalog and wait in the box, and its marks go.
func RecordMove(tx *sql.Tx, m Move, ids []int64) error {
	res, err := tx.Exec(`INSERT INTO quarantine_moves(batch, source_id, path, qpath, is_dir, nfiles, size, moved_at) VALUES (?,?,?,?,?,?,?,?)`,
		m.Batch, m.SID, m.Path, m.QPath, m.IsDir, m.Files, m.Size, m.MovedAt)
	if err != nil {
		return err
	}
	mid, _ := res.LastInsertId()
	for i := 0; i < len(ids); i += 500 {
		chunk := ids[i:min(i+500, len(ids))]
		args := make([]any, len(chunk))
		for j, id := range chunk {
			args[j] = id
		}
		ph := Placeholders(len(chunk))
		if _, err := tx.Exec(`INSERT INTO quarantine_files SELECT ?, path, name, ext, size, mtime, kind, top, sha256 FROM files WHERE id IN (`+ph+`)`,
			append([]any{mid}, args...)...); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM file_text WHERE rowid IN (`+ph+`)`, args...); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM files WHERE id IN (`+ph+`)`, args...); err != nil {
			return err
		}
	}
	_, err = tx.Exec(`DELETE FROM marks WHERE source_id=? AND (path=? OR substr(path,1,?)=?)`, m.SID, m.Path, len(m.Path)+1, m.Path+"/")
	return err
}

const moveCols = `id, batch, source_id, path, qpath, is_dir, nfiles, size, moved_at`

func scanMove(r interface{ Scan(...any) error }) (Move, error) {
	var m Move
	err := r.Scan(&m.ID, &m.Batch, &m.SID, &m.Path, &m.QPath, &m.IsDir, &m.Files, &m.Size, &m.MovedAt)
	return m, err
}

// Moves lists what waits in the farewell box, newest first.
func Moves(q Querier) ([]Move, error) {
	rows, err := q.Query(`SELECT ` + moveCols + ` FROM quarantine_moves WHERE state='moved' ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Move
	for rows.Next() {
		if m, err := scanMove(rows); err == nil {
			out = append(out, m)
		}
	}
	return out, rows.Err()
}

// MoveByID returns the move id if it still waits in the farewell box.
func MoveByID(q Querier, id string) (Move, error) {
	return scanMove(q.QueryRow(`SELECT `+moveCols+` FROM quarantine_moves WHERE id=? AND state='moved'`, id))
}

// RecordRestore notes that move m was put back where it was: its files return to the catalog.
func RecordRestore(tx *sql.Tx, m Move) error {
	if _, err := tx.Exec(`INSERT OR IGNORE INTO files(source_id, path, name, ext, size, mtime, kind, top, sha256, seen)
		SELECT ?, path, name, ext, size, mtime, kind, top, sha256, 1 FROM quarantine_files WHERE move_id=?`, m.SID, m.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM quarantine_files WHERE move_id=?`, m.ID); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE quarantine_moves SET state='restored' WHERE id=?`, m.ID)
	return err
}

// RecordDeleted notes that move id was deleted for good.
func RecordDeleted(q Querier, id int64) error {
	if _, err := q.Exec(`DELETE FROM quarantine_files WHERE move_id=?`, id); err != nil {
		return err
	}
	_, err := q.Exec(`UPDATE quarantine_moves SET state='deleted' WHERE id=?`, id)
	return err
}

// Placeholders is "?,?,…" for n arguments.
func Placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }
