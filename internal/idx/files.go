package idx

import "database/sql"

// File is one file row of an index.
type File struct {
	ID          int64
	Path        string
	Size, Mtime int64
	SHA         sql.NullString
}

func files(q Querier, query string, args ...any) ([]File, error) {
	rs, err := q.Query(`SELECT id, path, size, mtime, sha256 FROM files WHERE `+query, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []File
	for rs.Next() {
		var f File
		if rs.Scan(&f.ID, &f.Path, &f.Size, &f.Mtime, &f.SHA) == nil {
			out = append(out, f)
		}
	}
	return out, rs.Err()
}

// FileAt returns the file at rel of source sid, if the index has it.
func FileAt(q Querier, sid int64, rel string) ([]File, error) {
	return files(q, `source_id=? AND path=?`, sid, rel)
}

// FilesUnder returns the files inside folder rel of source sid, at any depth.
func FilesUnder(q Querier, sid int64, rel string) ([]File, error) {
	pre := rel + "/"
	return files(q, `source_id=? AND substr(path,1,?)=?`, sid, len(pre), pre)
}

// FilesAtOrUnder returns the file rel, or everything inside folder rel, of source sid.
func FilesAtOrUnder(q Querier, sid int64, rel string) ([]File, error) {
	return files(q, `source_id=? AND (path=? OR path LIKE ? ESCAPE '\')`, sid, rel, LikePrefix(rel))
}

// Text returns the searchable text kept for file id, if any.
func Text(q Querier, id int64) sql.NullString {
	var body sql.NullString
	q.QueryRow(`SELECT body FROM file_text WHERE rowid=?`, id).Scan(&body)
	return body
}
