#!/usr/bin/env python3
"""Merge another index into the main one.

    python3 merge.py index.db other.db            # import every source found in other.db
    python3 merge.py index.db other.db --dry-run  # show what would happen, change nothing
    python3 merge.py index.db other.db --label "Laptop" --rename "Laptop 2026"

Typical use: scan.py ran on another computer and produced other.db; this copies its
sources, files and extracted text into the main index. If the main index already has a
source with the same label, that source is REPLACED by the incoming data (that is how
you refresh a PC you scanned earlier). Marks are never touched. A backup copy of the
main index is written next to it first (index.db.bak) unless --no-backup is given.
"""
import argparse
import importlib.util
import os
import shutil
import sqlite3
import sys


sys.dont_write_bytecode = True  # do not litter the disk with __pycache__


def load_schema():
    here = os.path.dirname(os.path.abspath(__file__))
    spec = importlib.util.spec_from_file_location("sidecar_scan", os.path.join(here, "scan.py"))
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod.DDL, mod.SCHEMA_VERSION, mod


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("main_db")
    ap.add_argument("other_db")
    ap.add_argument("--label", action="append", default=[],
                    help="only import this source label (repeatable); default: all")
    ap.add_argument("--rename", action="append", default=[],
                    help="new label for the source selected by --label (same order)")
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--no-backup", action="store_true")
    a = ap.parse_args()

    if not os.path.exists(a.other_db):
        sys.exit(f"not found: {a.other_db}")
    if os.path.abspath(a.main_db) == os.path.abspath(a.other_db):
        sys.exit("main and other index are the same file")
    ddl, version, scan_mod = load_schema()

    other = sqlite3.connect(f"file:{os.path.abspath(a.other_db)}?mode=ro", uri=True)
    incoming = other.execute("SELECT id, label, root, host, scanned_at FROM sources ORDER BY id").fetchall()
    other.close()
    if a.label:
        incoming = [s for s in incoming if s[1] in a.label]
        missing = set(a.label) - {s[1] for s in incoming}
        if missing:
            sys.exit(f"no such source in {a.other_db}: {', '.join(sorted(missing))}")
    if not incoming:
        sys.exit("nothing to import")
    renames = dict(zip(a.label, a.rename)) if a.rename else {}

    if not a.dry_run and os.path.exists(a.main_db) and not a.no_backup:
        shutil.copy2(a.main_db, a.main_db + ".bak")

    con = sqlite3.connect(a.main_db, isolation_level=None)  # manual transactions
    con.executescript(ddl)
    scan_mod.check_schema(con)
    con.execute("INSERT OR REPLACE INTO meta VALUES ('schema_version', ?)", (str(version),))
    con.execute("ATTACH DATABASE ? AS o", (f"file:{os.path.abspath(a.other_db)}?mode=ro",))

    con.execute("BEGIN")
    for osid, label, root, host, scanned_at in incoming:
        new_label = renames.get(label, label)
        n_in, s_in = con.execute("SELECT COUNT(*), COALESCE(SUM(size),0) FROM o.files WHERE source_id=?", (osid,)).fetchone()
        row = con.execute("SELECT id FROM sources WHERE label=?", (new_label,)).fetchone()
        action = "replace" if row else "add"
        print(f"{action:8} '{new_label}' <- '{label}': {n_in} files, {s_in / 1e9:.1f} GB")
        if row:
            sid = row[0]
            con.execute("DELETE FROM file_text WHERE rowid IN (SELECT id FROM files WHERE source_id=?)", (sid,))
            con.execute("DELETE FROM files WHERE source_id=?", (sid,))
            con.execute("UPDATE sources SET root=?, host=?, scanned_at=? WHERE id=?", (root, host, scanned_at, sid))
        else:
            sid = con.execute("INSERT INTO sources(label, root, host, scanned_at) VALUES (?,?,?,?)",
                              (new_label, root, host, scanned_at)).lastrowid
        con.execute(
            "INSERT INTO files(source_id, path, name, ext, size, mtime, kind, top, sha256, seen) "
            "SELECT ?, path, name, ext, size, mtime, kind, top, sha256, 1 FROM o.files WHERE source_id=?",
            (sid, osid))
        try:
            n_text = con.execute(
                "INSERT INTO file_text(rowid, path, body) "
                "SELECT m.id, t.path, t.body FROM o.file_text t JOIN o.files sf ON sf.id = t.rowid "
                "JOIN files m ON m.source_id = ? AND m.path = sf.path WHERE sf.source_id = ?",
                (sid, osid)).rowcount
        except sqlite3.OperationalError:  # other index has no text table
            n_text = 0
        print(f"         text of {n_text} files imported")

    if a.dry_run:
        con.execute("ROLLBACK")
        print("dry run: nothing written")
    else:
        con.execute("COMMIT")
        tot, size = con.execute("SELECT COUNT(*), COALESCE(SUM(size),0) FROM files").fetchone()
        print(f"done. main index now: {tot} files, {size / 1e9:.1f} GB. Backup: {a.main_db}.bak")
    con.close()


if __name__ == "__main__":
    main()
