#!/usr/bin/env python3
"""Confirm duplicates: SHA-256 only files that share their size with another file.

    python3 hash_dups.py --db index.db [--min-mb 1] [--also-match other.db]

--also-match OTHER.db  also hash files whose size occurs in OTHER.db (for example the
                       main disk index), so duplicates across two machines are found
                       even though each machine hashes separately.

Reads each source root recorded by scan.py (sources.root). Resumable: files
that already have a hash are skipped. Read-only on the disk.
"""
import argparse
import hashlib
import os
import sqlite3
import sys
import time


def sha256_of(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        while chunk := f.read(4 << 20):
            h.update(chunk)
    return h.hexdigest()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--db", default="index.db")
    ap.add_argument("--min-mb", type=float, default=1.0)
    ap.add_argument("--also-match", help="another index.db whose file sizes count as 'ties'")
    ap.add_argument("--root", action="append", default=[],
                    help="override a source root: LABEL=/path (use when a disk is mounted elsewhere)")
    a = ap.parse_args()
    min_size = int(a.min_mb * 1024 * 1024)
    overrides = dict(r.split("=", 1) for r in a.root)

    con = sqlite3.connect(a.db, uri=True) if a.also_match else sqlite3.connect(a.db)
    roots = {sid: overrides.get(label, root) for sid, label, root in
             con.execute("SELECT id, label, root FROM sources")}
    for sid, root in roots.items():
        if not os.path.isdir(root):
            print(f"warning: root not found for source {sid}: {root} (its files are skipped)", file=sys.stderr)
    ties = "SELECT size FROM files WHERE size >= ? GROUP BY size HAVING COUNT(*) > 1"
    params = [min_size, min_size]
    if a.also_match:
        con.execute("ATTACH DATABASE ? AS other", (f"file:{os.path.abspath(a.also_match)}?mode=ro",))
        ties += " UNION SELECT size FROM other.files WHERE size >= ?"
        params.append(min_size)
    rows = con.execute(
        f"SELECT id, source_id, path, size FROM files WHERE sha256 IS NULL AND size >= ? "
        f"AND size IN ({ties}) ORDER BY size DESC", [min_size] + params[1:]).fetchall()
    rows = [r for r in rows if os.path.isdir(roots[r[1]])]
    total = sum(r[3] for r in rows)
    print(f"{len(rows)} files to hash, {total / 1e9:.1f} GB", file=sys.stderr)

    done = nbytes = errors = 0
    t0 = time.time()
    for fid, sid, rel, size in rows:
        full = os.path.join(roots[sid], rel)
        try:
            digest = sha256_of(full)
        except OSError as e:
            errors += 1
            print(f"error {rel}: {e}", file=sys.stderr)
            continue
        con.execute("UPDATE files SET sha256 = ? WHERE id = ?", (digest, fid))
        done += 1
        nbytes += size
        if done % 100 == 0:
            con.commit()
            el = time.time() - t0
            print(f"  {done}/{len(rows)} files, {nbytes / 1e9:.1f}/{total / 1e9:.1f} GB, "
                  f"{nbytes / 1e6 / el:.0f} MB/s", file=sys.stderr)
    con.commit()

    groups, wasted = con.execute(
        "SELECT COUNT(*), COALESCE(SUM(w), 0) FROM (SELECT size * (COUNT(*) - 1) w FROM files "
        "WHERE sha256 IS NOT NULL GROUP BY sha256 HAVING COUNT(*) > 1)").fetchone()
    con.close()
    print(f"hashed {done} files ({errors} errors) in {time.time() - t0:.0f}s | "
          f"confirmed duplicate groups: {groups}, recoverable: {wasted / 1e9:.1f} GB")


if __name__ == "__main__":
    main()
