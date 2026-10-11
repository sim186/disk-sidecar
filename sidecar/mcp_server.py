#!/usr/bin/env python3
"""Disk Sidecar MCP server (stdio, no dependencies).

Gives an LLM read-only search over the disk index (index.db).

    SIDECAR_DB=/Volumes/MyDisk/.sidecar/index.db python3 mcp_server.py

If SIDECAR_DB is not set, index.db next to this script is used.
Register with Claude Code:
    claude mcp add disk-sidecar -- python3 /Volumes/MyDisk/.sidecar/mcp_server.py
"""
import json
import os
import sqlite3
import sys
from pathlib import Path

DB = os.environ.get("SIDECAR_DB") or str(Path(__file__).resolve().parent / "index.db")
PROTOCOL = "2024-11-05"


def connect():
    con = sqlite3.connect(Path(DB).resolve().as_uri() + "?mode=ro", uri=True)
    con.row_factory = sqlite3.Row
    return con


def rows(sql, params=()):
    con = connect()
    try:
        return [dict(r) for r in con.execute(sql, params)]
    finally:
        con.close()


def src_clause(source, col="f.source_id"):
    if not source:
        return "", []
    return f" AND {col} IN (SELECT id FROM sources WHERE label = ?)", [source]


def label_join():
    return "JOIN sources s ON s.id = f.source_id"


def search_files(query, kind=None, source=None, limit=30):
    """Search by file or folder name (substring, case-insensitive)."""
    like = "%" + query.replace("\\", "\\\\").replace("%", "\\%").replace("_", "\\_") + "%"
    sql = ("SELECT s.label AS source, f.path, f.size, f.kind, f.mtime FROM files f " + label_join() +
           " WHERE f.path LIKE ? ESCAPE '\\'")
    p = [like]
    if kind:
        sql += " AND f.kind = ?"; p.append(kind)
    c, cp = src_clause(source)
    sql += c + " ORDER BY f.size DESC LIMIT ?"
    return rows(sql, p + cp + [min(int(limit), 200)])


def search_content(query, source=None, limit=20):
    """Full-text search inside txt/md/code/pdf contents. Returns snippets."""
    terms = " ".join('"' + t.replace('"', '""') + '"*' for t in query.split())
    sql = ("SELECT s.label AS source, f.path, f.size, f.kind, "
           "snippet(file_text, 1, '[', ']', ' … ', 12) AS snippet "
           "FROM file_text t JOIN files f ON f.id = t.rowid " + label_join() +
           " WHERE file_text MATCH ?")
    c, cp = src_clause(source)
    return rows(sql + c + " ORDER BY rank LIMIT ?", [terms] + cp + [min(int(limit), 100)])


def get_file_info(path, source=None):
    """Details of one file, plus other files with the same SHA-256."""
    c, cp = src_clause(source)
    found = rows("SELECT s.label AS source, f.path, f.size, f.kind, f.mtime, f.sha256 FROM files f " +
                 label_join() + " WHERE f.path = ?" + c, [path] + cp)
    for r in found:
        r["copies"] = rows("SELECT s.label AS source, f.path FROM files f " + label_join() +
                           " WHERE f.sha256 = ? AND NOT (f.path = ? AND s.label = ?)",
                           [r["sha256"], r["path"], r["source"]]) if r["sha256"] else []
    return found


def list_folder(path="", source=None, limit=100):
    """List immediate children of a folder with file counts and sizes."""
    pre = path.strip("/") + "/" if path.strip("/") else ""
    c, cp = src_clause(source)
    return rows(
        "SELECT name, isdir, COUNT(*) files, SUM(size) size FROM ("
        "SELECT size, CASE WHEN instr(seg,'/')>0 THEN substr(seg,1,instr(seg,'/')-1) ELSE seg END name, "
        "instr(seg,'/')>0 isdir FROM (SELECT f.size, substr(f.path, ?) seg FROM files f "
        "WHERE substr(f.path,1,?) = ?" + c + ")) GROUP BY name, isdir ORDER BY size DESC LIMIT ?",
        [len(pre) + 1, len(pre), pre] + cp + [min(int(limit), 500)])


def find_duplicates(limit=20, folder=None):
    """Confirmed duplicate groups (same SHA-256), largest recoverable space first."""
    p, w = [], ""
    if folder:
        pre = folder.strip("/") + "/"
        w = " AND substr(path,1,?) = ?"; p = [len(pre), pre]
    groups = rows("SELECT sha256, COUNT(*) copies, size, size*(COUNT(*)-1) recoverable FROM files "
                  "WHERE sha256 IS NOT NULL" + w + " GROUP BY sha256 HAVING copies > 1 "
                  "ORDER BY recoverable DESC LIMIT ?", p + [min(int(limit), 100)])
    for g in groups:
        g["files"] = rows("SELECT s.label AS source, f.path FROM files f " + label_join() +
                          " WHERE f.sha256 = ?", [g["sha256"]])
    return groups


def disk_summary():
    """Sources, totals by kind and by top-level folder."""
    return {
        "sources": rows("SELECT s.label, s.root, s.host, s.scanned_at, COUNT(f.id) files, SUM(f.size) size "
                        "FROM sources s LEFT JOIN files f ON f.source_id = s.id GROUP BY s.id"),
        "by_kind": rows("SELECT kind, COUNT(*) files, SUM(size) size FROM files GROUP BY kind ORDER BY size DESC"),
        "top_folders": rows("SELECT top, COUNT(*) files, SUM(size) size FROM files GROUP BY top "
                            "ORDER BY size DESC LIMIT 20"),
    }


S = lambda **props: {"type": "object", "properties": props}
STR = {"type": "string"}
INT = {"type": "integer"}
TOOLS = {
    "disk_summary": (disk_summary, "Overview of the indexed disks: sources, sizes by type and top folders.", S()),
    "search_files": (search_files, "Find files/folders by name. Paths are relative to the source root.",
                     {**S(query=STR, kind={"type": "string", "description": "photo|video|audio|document|code|installer|archive|lightroom|other"},
                          source=STR, limit=INT), "required": ["query"]}),
    "search_content": (search_content, "Full-text search inside text, code and PDF files; returns snippets.",
                       {**S(query=STR, source=STR, limit=INT), "required": ["query"]}),
    "get_file_info": (get_file_info, "Size, date, kind, hash and duplicate copies of one file (exact relative path).",
                      {**S(path=STR, source=STR), "required": ["path"]}),
    "list_folder": (list_folder, "List a folder's children with sizes ('' = root).", S(path=STR, source=STR, limit=INT)),
    "find_duplicates": (find_duplicates, "Confirmed duplicate groups, biggest recoverable space first.", S(limit=INT, folder=STR)),
}


def handle(msg):
    method, mid = msg.get("method"), msg.get("id")
    if method == "initialize":
        return {"protocolVersion": PROTOCOL, "capabilities": {"tools": {}},
                "serverInfo": {"name": "disk-sidecar", "version": "0.1.0"}}
    if method == "tools/list":
        return {"tools": [{"name": n, "description": d, "inputSchema": s} for n, (_, d, s) in TOOLS.items()]}
    if method == "tools/call":
        p = msg["params"]
        fn = TOOLS[p["name"]][0]
        try:
            out = fn(**(p.get("arguments") or {}))
            return {"content": [{"type": "text", "text": json.dumps(out, ensure_ascii=False, indent=1)}]}
        except Exception as e:  # report to the model instead of crashing the server
            return {"isError": True, "content": [{"type": "text", "text": f"{type(e).__name__}: {e}"}]}
    if method == "ping":
        return {}
    if mid is None:
        return None  # notification
    raise KeyError(method)


def main():
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        msg = json.loads(line)
        try:
            result = handle(msg)
        except KeyError as e:
            if msg.get("id") is not None:
                print(json.dumps({"jsonrpc": "2.0", "id": msg["id"], "error": {"code": -32601, "message": f"unknown method {e}"}}), flush=True)
            continue
        if msg.get("id") is not None and result is not None:
            print(json.dumps({"jsonrpc": "2.0", "id": msg["id"], "result": result}), flush=True)


if __name__ == "__main__":
    main()
