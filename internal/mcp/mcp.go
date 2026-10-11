// Package mcp is a small MCP server (JSON-RPC over stdio) that lets an LLM search a disk's index.
package mcp

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sim186/disk-sidecar/internal/assets"
	"github.com/sim186/disk-sidecar/internal/idx"
)

type row = map[string]any

func queryMaps(db *sql.DB, q string, args ...any) ([]row, error) {
	rs, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	cols, _ := rs.Columns()
	out := []row{}
	for rs.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rs.Scan(ptrs...); err != nil {
			return nil, err
		}
		r := row{}
		for i, c := range cols {
			if b, ok := vals[i].([]byte); ok {
				vals[i] = string(b)
			}
			r[c] = vals[i]
		}
		out = append(out, r)
	}
	return out, rs.Err()
}

func str(a map[string]any, k string) string {
	if v, ok := a[k].(string); ok {
		return v
	}
	return ""
}

func num(a map[string]any, k string, def, max int) int {
	n := def
	if v, ok := a[k].(float64); ok {
		n = int(v)
	}
	if n > max {
		n = max
	}
	if n < 1 {
		n = def
	}
	return n
}

func srcClause(source string) (string, []any) {
	if source == "" {
		return "", nil
	}
	return ` AND f.source_id IN (SELECT id FROM sources WHERE label = ?)`, []any{source}
}

type tool struct {
	desc   string
	schema map[string]any
	run    func(db *sql.DB, a map[string]any) (any, error)
}

func props(p map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": p}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

var (
	tString = map[string]any{"type": "string"}
	tInt    = map[string]any{"type": "integer"}
)

var tools = map[string]tool{
	"disk_summary": {"Overview of the indexed disks: sources, sizes by type and top folders.", props(map[string]any{}),
		func(db *sql.DB, a map[string]any) (any, error) {
			srcs, err := queryMaps(db, `SELECT s.label, s.root, s.host, s.scanned_at, COUNT(f.id) files, SUM(f.size) size FROM sources s LEFT JOIN files f ON f.source_id = s.id GROUP BY s.id`)
			if err != nil {
				return nil, err
			}
			kinds, _ := queryMaps(db, `SELECT kind, COUNT(*) files, SUM(size) size FROM files GROUP BY kind ORDER BY size DESC`)
			tops, _ := queryMaps(db, `SELECT top, COUNT(*) files, SUM(size) size FROM files GROUP BY top ORDER BY size DESC LIMIT 20`)
			return row{"sources": srcs, "by_kind": kinds, "top_folders": tops}, nil
		}},
	"search_files": {"Find files/folders by name. Paths are relative to the source root.",
		props(map[string]any{"query": tString, "kind": map[string]any{"type": "string", "description": "photo|video|audio|document|code|installer|archive|lightroom|other"}, "source": tString, "limit": tInt}, "query"),
		func(db *sql.DB, a map[string]any) (any, error) {
			like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(str(a, "query")) + "%"
			q := `SELECT s.label AS source, f.path, f.size, f.kind, f.mtime FROM files f JOIN sources s ON s.id = f.source_id WHERE f.path LIKE ? ESCAPE '\'`
			args := []any{like}
			if k := str(a, "kind"); k != "" {
				q += ` AND f.kind = ?`
				args = append(args, k)
			}
			c, cp := srcClause(str(a, "source"))
			return queryMaps(db, q+c+` ORDER BY f.size DESC LIMIT ?`, append(append(args, cp...), num(a, "limit", 30, 200))...)
		}},
	"search_content": {"Full-text search inside text, code and PDF files; returns snippets.",
		props(map[string]any{"query": tString, "source": tString, "limit": tInt}, "query"),
		func(db *sql.DB, a map[string]any) (any, error) {
			var terms []string
			for _, t := range strings.Fields(str(a, "query")) {
				terms = append(terms, `"`+strings.ReplaceAll(t, `"`, `""`)+`"*`)
			}
			c, cp := srcClause(str(a, "source"))
			return queryMaps(db, `SELECT s.label AS source, f.path, f.size, f.kind, snippet(file_text, 1, '[', ']', ' … ', 12) AS snippet
				FROM file_text t JOIN files f ON f.id = t.rowid JOIN sources s ON s.id = f.source_id WHERE file_text MATCH ?`+c+` ORDER BY rank LIMIT ?`,
				append(append([]any{strings.Join(terms, " ")}, cp...), num(a, "limit", 20, 100))...)
		}},
	"get_file_info": {"Size, date, kind, hash and duplicate copies of one file (exact relative path).",
		props(map[string]any{"path": tString, "source": tString}, "path"),
		func(db *sql.DB, a map[string]any) (any, error) {
			c, cp := srcClause(str(a, "source"))
			found, err := queryMaps(db, `SELECT s.label AS source, f.path, f.size, f.kind, f.mtime, f.sha256 FROM files f JOIN sources s ON s.id = f.source_id WHERE f.path = ?`+c,
				append([]any{str(a, "path")}, cp...)...)
			if err != nil {
				return nil, err
			}
			for _, r := range found {
				r["copies"] = []row{}
				if h, ok := r["sha256"].(string); ok && h != "" {
					copies, _ := queryMaps(db, `SELECT s.label AS source, f.path FROM files f JOIN sources s ON s.id = f.source_id WHERE f.sha256 = ? AND NOT (f.path = ? AND s.label = ?)`, h, r["path"], r["source"])
					r["copies"] = copies
				}
			}
			return found, nil
		}},
	"list_folder": {"List a folder's children with sizes ('' = root).", props(map[string]any{"path": tString, "source": tString, "limit": tInt}),
		func(db *sql.DB, a map[string]any) (any, error) {
			pre := ""
			if p := strings.Trim(str(a, "path"), "/"); p != "" {
				pre = p + "/"
			}
			c, cp := srcClause(str(a, "source"))
			return queryMaps(db, `SELECT name, isdir, COUNT(*) files, SUM(size) size FROM (
				SELECT size, CASE WHEN instr(seg,'/')>0 THEN substr(seg,1,instr(seg,'/')-1) ELSE seg END name, instr(seg,'/')>0 isdir
				FROM (SELECT f.size, substr(f.path, ?) seg FROM files f WHERE substr(f.path,1,?) = ?`+c+`)) GROUP BY name, isdir ORDER BY size DESC LIMIT ?`,
				append(append([]any{len(pre) + 1, len(pre), pre}, cp...), num(a, "limit", 100, 500))...)
		}},
	"find_duplicates": {"Confirmed duplicate groups, biggest recoverable space first.", props(map[string]any{"limit": tInt, "folder": tString}),
		func(db *sql.DB, a map[string]any) (any, error) {
			w, args := "", []any{}
			if f := strings.Trim(str(a, "folder"), "/"); f != "" {
				w, args = ` AND substr(path,1,?) = ?`, []any{len(f) + 1, f + "/"}
			}
			groups, err := queryMaps(db, `SELECT sha256, COUNT(*) copies, size, size*(COUNT(*)-1) recoverable FROM files WHERE sha256 IS NOT NULL`+w+
				` GROUP BY sha256 HAVING copies > 1 ORDER BY recoverable DESC LIMIT ?`, append(args, num(a, "limit", 20, 100))...)
			if err != nil {
				return nil, err
			}
			for _, g := range groups {
				g["files"], _ = queryMaps(db, `SELECT s.label AS source, f.path FROM files f JOIN sources s ON s.id = f.source_id WHERE f.sha256 = ?`, g["sha256"])
			}
			return groups, nil
		}},
}

// DefaultDB is the index of the disk this program is installed on.
func DefaultDB() string {
	if v := os.Getenv("SIDECAR_DB"); v != "" {
		return v
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	for dir := filepath.Dir(exe); ; dir = filepath.Dir(dir) {
		if filepath.Base(dir) == ".sidecar" {
			return filepath.Join(dir, "index.db")
		}
		if filepath.Dir(dir) == dir {
			return ""
		}
	}
}

type request struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	} `json:"params"`
}

// Serve answers MCP requests from in until it ends. The index is opened read-only.
func Serve(dbPath string, in io.Reader, out io.Writer) error {
	if dbPath == "" {
		return fmt.Errorf("no index: set SIDECAR_DB or use --db, or run this from <disk>/.sidecar")
	}
	db, err := idx.OpenRO(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	reply := func(id json.RawMessage, result any) {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
		fmt.Fprintf(out, "%s\n", b)
	}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req request
		if json.Unmarshal([]byte(line), &req) != nil {
			continue
		}
		hasID := len(req.ID) > 0 && string(req.ID) != "null"
		switch req.Method {
		case "initialize":
			reply(req.ID, row{"protocolVersion": "2024-11-05", "capabilities": row{"tools": row{}},
				"serverInfo": row{"name": "disk-sidecar", "version": assets.Version}})
		case "tools/list":
			var list []row
			for n, t := range tools {
				list = append(list, row{"name": n, "description": t.desc, "inputSchema": t.schema})
			}
			reply(req.ID, row{"tools": list})
		case "tools/call":
			t, ok := tools[req.Params.Name]
			if !ok {
				reply(req.ID, row{"isError": true, "content": []row{{"type": "text", "text": "unknown tool: " + req.Params.Name}}})
				continue
			}
			res, err := t.run(db, req.Params.Arguments)
			if err != nil {
				reply(req.ID, row{"isError": true, "content": []row{{"type": "text", "text": err.Error()}}})
				continue
			}
			b, _ := json.MarshalIndent(res, "", " ")
			reply(req.ID, row{"content": []row{{"type": "text", "text": string(b)}}})
		case "ping":
			reply(req.ID, row{})
		default:
			if hasID {
				b, _ := json.Marshal(row{"jsonrpc": "2.0", "id": req.ID, "error": row{"code": -32601, "message": "unknown method " + req.Method}})
				fmt.Fprintf(out, "%s\n", b)
			}
		}
	}
	return sc.Err()
}
