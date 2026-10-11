#!/usr/bin/env python3
"""Disk Sidecar scanner.

Walks a directory tree (read-only) and records every file in a SQLite
index. Incremental: files whose size and mtime did not change are skipped.

    python3 scan.py <root> --db index.db --source "My disk"
"""
import argparse
import hashlib
import os
import sqlite3
import subprocess
import sys
import time
import uuid

SCHEMA_VERSION = 1   # bump when the index layout changes in a way older programs cannot handle
HERE = os.path.dirname(os.path.abspath(__file__))


def program_version():
    try:
        with open(os.path.join(HERE, "VERSION"), encoding="utf-8") as f:
            return f.read().strip()
    except OSError:
        return "unknown"


def check_schema(con):
    """Refuse to touch an index written by a newer Disk Sidecar than this one."""
    try:
        row = con.execute("SELECT value FROM meta WHERE key='schema_version'").fetchone()
    except sqlite3.OperationalError:
        return
    if row and int(row[0]) > SCHEMA_VERSION:
        raise SystemExit(f"This index uses schema {row[0]}, newer than this program supports ({SCHEMA_VERSION}). "
                         f"Update Disk Sidecar (this is version {program_version()}).")

KINDS = {
    "photo": {"jpg", "jpeg", "png", "heic", "gif", "tif", "tiff", "dng", "nef", "arw",
              "cr2", "cr3", "raf", "orf", "rw2", "webp", "bmp", "psd", "xmp"},
    "video": {"mkv", "mp4", "mov", "avi", "m4v", "wmv", "mpg", "mpeg", "mts", "m2ts", "flv", "webm"},
    "audio": {"mp3", "flac", "wav", "m4a", "aac", "ogg", "aiff"},
    "document": {"pdf", "doc", "docx", "xls", "xlsx", "ppt", "pptx", "odt", "ods", "txt",
                 "md", "rtf", "csv", "epub", "pages", "numbers", "key"},
    "code": {"py", "js", "ts", "tsx", "jsx", "c", "h", "cpp", "hpp", "cs", "java", "kt",
             "swift", "go", "rs", "ino", "html", "css", "json", "yml", "yaml", "xml",
             "sh", "sql", "php", "rb", "class", "pyc"},
    "installer": {"exe", "msi", "dmg", "pkg", "iso", "deb", "apk", "inf", "dll", "sys"},
    "archive": {"zip", "rar", "7z", "tar", "gz", "tgz", "bz2", "xz"},
    "lightroom": {"lrcat", "lrdata", "lrprev", "lrfprev", "lrtemplate"},
}
EXT_KIND = {e: k for k, exts in KINDS.items() for e in exts}

TEXT_EXTS = {"txt", "md", "csv", "py", "js", "ts", "tsx", "jsx", "c", "h", "cpp", "hpp",
             "cs", "java", "kt", "swift", "go", "rs", "ino", "html", "css", "json", "yml",
             "yaml", "xml", "sh", "sql", "php", "rb"}
MAX_TEXT_BYTES = 512 * 1024
SKIP_DIRS = {"$RECYCLE.BIN", "System Volume Information", ".Spotlight-V100", ".Trashes",
             ".fseventsd", ".TemporaryItems", ".sidecar"}
SKIP_FILES = {".DS_Store", "Thumbs.db", "desktop.ini"}

DDL = """
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT);
CREATE TABLE IF NOT EXISTS sources (
    id INTEGER PRIMARY KEY, label TEXT UNIQUE NOT NULL, root TEXT, host TEXT, scanned_at INTEGER);
CREATE TABLE IF NOT EXISTS files (
    id INTEGER PRIMARY KEY,
    source_id INTEGER NOT NULL REFERENCES sources(id),
    path TEXT NOT NULL,            -- relative to the source root, '/' separated
    name TEXT NOT NULL,
    ext TEXT,
    size INTEGER NOT NULL,
    mtime INTEGER NOT NULL,
    kind TEXT NOT NULL,
    top TEXT,                      -- first path component, for quick grouping
    sha256 TEXT,
    seen INTEGER NOT NULL DEFAULT 0,
    UNIQUE (source_id, path));
CREATE INDEX IF NOT EXISTS files_size ON files(size);
CREATE INDEX IF NOT EXISTS files_hash ON files(sha256);
CREATE INDEX IF NOT EXISTS files_kind ON files(kind);
CREATE INDEX IF NOT EXISTS files_top ON files(source_id, top);
CREATE TABLE IF NOT EXISTS marks (
    source_id INTEGER NOT NULL, path TEXT NOT NULL,  -- file or folder
    mark TEXT NOT NULL CHECK (mark IN ('keep','delete','review')), note TEXT,
    PRIMARY KEY (source_id, path));
CREATE VIRTUAL TABLE IF NOT EXISTS file_text USING fts5(
    path, body, tokenize='unicode61 remove_diacritics 2');
"""


def kind_of(ext):
    return EXT_KIND.get(ext, "other")


def read_text(full, ext):
    """Return extracted text or None."""
    try:
        if ext in TEXT_EXTS:
            with open(full, "rb") as f:
                return f.read(MAX_TEXT_BYTES).decode("utf-8", "replace")
        if ext == "pdf":
            out = subprocess.run(["pdftotext", "-q", "-l", "30", full, "-"],
                                 capture_output=True, timeout=30)
            return out.stdout[:MAX_TEXT_BYTES].decode("utf-8", "replace")
    except (OSError, subprocess.SubprocessError):
        return None
    return None


def sha256_of(full):
    h = hashlib.sha256()
    with open(full, "rb") as f:
        while chunk := f.read(1 << 20):
            h.update(chunk)
    return h.hexdigest()


def scan(root, db_path, label, host, hash_min, text, progress=None, quiet=False):
    root = os.path.abspath(root)
    con = sqlite3.connect(db_path)
    con.executescript(DDL)
    check_schema(con)
    con.execute("INSERT OR REPLACE INTO meta VALUES ('schema_version', ?)", (str(SCHEMA_VERSION),))
    con.execute("INSERT OR REPLACE INTO meta VALUES ('sidecar_version', ?)", (program_version(),))
    con.execute("INSERT OR IGNORE INTO meta VALUES ('disk_id', ?)", (uuid.uuid4().hex,))
    con.execute("INSERT INTO sources(label, root, host) VALUES (?,?,?) "
                "ON CONFLICT(label) DO UPDATE SET root=excluded.root, host=excluded.host",
                (label, root, host))
    sid = con.execute("SELECT id FROM sources WHERE label=?", (label,)).fetchone()[0]
    con.execute("UPDATE files SET seen=0 WHERE source_id=?", (sid,))

    known = {p: (s, m) for p, s, m in
             con.execute("SELECT path, size, mtime FROM files WHERE source_id=?", (sid,))}
    stats = dict(new=0, changed=0, same=0, errors=0)
    t0 = time.time()
    rowid_for_text = {}

    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = [d for d in dirnames if d not in SKIP_DIRS]
        rel_dir = os.path.relpath(dirpath, root)
        rel_dir = "" if rel_dir == "." else rel_dir.replace(os.sep, "/")
        for name in filenames:
            if name in SKIP_FILES or name.startswith("._"):
                continue
            rel = f"{rel_dir}/{name}" if rel_dir else name
            full = os.path.join(dirpath, name)
            try:
                st = os.stat(full)
            except OSError as e:
                stats["errors"] += 1
                print(f"stat error {rel}: {e}", file=sys.stderr)
                continue
            size, mtime = st.st_size, int(st.st_mtime)
            if known.get(rel) == (size, mtime):
                con.execute("UPDATE files SET seen=1 WHERE source_id=? AND path=?", (sid, rel))
                stats["same"] += 1
                continue
            ext = os.path.splitext(name)[1].lstrip(".").lower()
            digest = None
            if hash_min is not None and size >= hash_min:
                try:
                    digest = sha256_of(full)
                except OSError as e:
                    stats["errors"] += 1
                    print(f"hash error {rel}: {e}", file=sys.stderr)
            top = rel.split("/", 1)[0] if "/" in rel else ""
            stats["changed" if rel in known else "new"] += 1
            con.execute(
                "INSERT INTO files(source_id,path,name,ext,size,mtime,kind,top,sha256,seen) "
                "VALUES (?,?,?,?,?,?,?,?,?,1) ON CONFLICT(source_id,path) DO UPDATE SET "
                "size=excluded.size, mtime=excluded.mtime, kind=excluded.kind, top=excluded.top, "
                "sha256=excluded.sha256, seen=1",
                (sid, rel, name, ext, size, mtime, kind_of(ext), top, digest))
            if text:
                body = read_text(full, ext)
                if body:
                    fid = con.execute("SELECT id FROM files WHERE source_id=? AND path=?",
                                      (sid, rel)).fetchone()[0]
                    rowid_for_text[fid] = (rel, body)
            n = stats["new"] + stats["changed"] + stats["same"]
            if progress and n % 500 == 0:
                progress(n)
            if n % 5000 == 0:
                con.commit()
                if not quiet:
                    print(f"  {n} files ({time.time() - t0:.0f}s)", file=sys.stderr)

    for fid, (rel, body) in rowid_for_text.items():
        con.execute("DELETE FROM file_text WHERE rowid=?", (fid,))
        con.execute("INSERT INTO file_text(rowid, path, body) VALUES (?,?,?)", (fid, rel, body))

    gone = con.execute("SELECT COUNT(*) FROM files WHERE source_id=? AND seen=0", (sid,)).fetchone()[0]
    con.execute("DELETE FROM file_text WHERE rowid IN (SELECT id FROM files WHERE source_id=? AND seen=0)", (sid,))
    con.execute("DELETE FROM files WHERE source_id=? AND seen=0", (sid,))
    con.execute("UPDATE sources SET scanned_at=? WHERE id=?", (int(time.time()), sid))
    con.commit()
    total, tsize = con.execute("SELECT COUNT(*), COALESCE(SUM(size),0) FROM files WHERE source_id=?",
                               (sid,)).fetchone()
    con.close()
    secs = time.time() - t0
    if not quiet:
        print(f"[{label}] {total} files, {tsize / 1e9:.1f} GB | new {stats['new']}, "
              f"changed {stats['changed']}, same {stats['same']}, removed {gone}, "
              f"errors {stats['errors']} | {secs:.0f}s")
    return dict(total=total, size=tsize, new=stats["new"], changed=stats["changed"], same=stats["same"],
                removed=gone, errors=stats["errors"], secs=secs)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--version", action="version", version=f"disk-sidecar scan {program_version()}")
    ap.add_argument("root")
    ap.add_argument("--db", default="index.db")
    ap.add_argument("--source", required=True, help="label for this source (disk / PC)")
    ap.add_argument("--hash-min-mb", type=float, default=None,
                    help="also SHA-256 files >= this size (for duplicate detection)")
    ap.add_argument("--no-text", action="store_true", help="skip full-text extraction")
    a = ap.parse_args()
    hash_min = None if a.hash_min_mb is None else int(a.hash_min_mb * 1024 * 1024)
    scan(a.root, a.db, a.source, os.uname().nodename if hasattr(os, "uname") else os.environ.get("COMPUTERNAME", ""),
         hash_min, not a.no_text)


if __name__ == "__main__":
    main()
