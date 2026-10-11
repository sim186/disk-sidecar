#!/usr/bin/env python3
"""Disk Sidecar local server: serves the app, keeps indexes fresh, moves files to quarantine.

    python3 serve.py [--port 8765] [--no-open] [--no-verify] [--mounts DIR ...]

Model: every disk carries its own index, <disk>/.sidecar/index.db, describing only that disk.
This server (started from any disk's .sidecar) looks for all mounted disks that have one and
shows them together. Each disk's index is written only about its own files: scans, hashes,
marks (keep / delete / review) and quarantine all go to the index of the disk the file lives on.
Snapshots of computers you cannot plug in (<disk>/.sidecar/peers/*.db) are shown read-only.
Comparisons between disks are never stored: they are computed live from the indexes.

On start it re-checks every mounted disk against its index (a quick incremental scan) and
tells the page what changed by hand since last time.

Serves on 127.0.0.1 only. Actions need a random token the page receives from this server:
  * marks       save keep/delete/review notes in the owning disk's index
  * quarantine  move marked files into <disk>/.sidecar/_quarantena/<date>/<source>/<path>
                (a rename on the same disk: instant, reversible, nothing is deleted)
  * restore     move a quarantined item back
  * empty       permanently delete all quarantines (needs the typed word ELIMINA)
  * verify      re-check disks now
Long actions run in the background; the page shows a progress bar.

Refused whatever the browser asks: paths outside a disk root, files changed since the last
scan, and removing the last remaining copy (same SHA-256) of a file across the mounted disks.
Every action is appended to log.csv next to this script.
"""
import argparse
import csv
import hashlib
import http.server
import importlib.util
import json
import os
import platform
import posixpath
import re
import secrets
import shutil
import socketserver
import sqlite3
import string
import sys
import tempfile
import threading
import time
import urllib.parse
import webbrowser
from pathlib import Path

sys.dont_write_bytecode = True  # do not litter the disk with __pycache__
HERE = Path(__file__).resolve().parent
QDIR = ".sidecar/_quarantena"
LOCK = threading.Lock()          # one writing action at a time
INDEX_LOCK = threading.Lock()
GEN = [0]                        # bumped after every change we make, to refresh the page's copy
EXTRA_MOUNTS = []
NO_DEFAULT_MOUNTS = [False]
HOME_DB = [None]                 # explicit --db for development


def _load_scan():
    spec = importlib.util.spec_from_file_location("sidecar_scan", HERE / "scan.py")
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


SCAN = _load_scan()

DDL = """
CREATE TABLE IF NOT EXISTS quarantine_moves (
    id INTEGER PRIMARY KEY, batch TEXT, source_id INTEGER, path TEXT, qpath TEXT,
    is_dir INTEGER, nfiles INTEGER, size INTEGER, moved_at INTEGER, state TEXT DEFAULT 'moved');
CREATE TABLE IF NOT EXISTS quarantine_files (
    move_id INTEGER, path TEXT, name TEXT, ext TEXT, size INTEGER, mtime INTEGER,
    kind TEXT, top TEXT, sha256 TEXT);
CREATE INDEX IF NOT EXISTS quarantine_files_move ON quarantine_files(move_id);
CREATE TABLE IF NOT EXISTS scan_log (
    time INTEGER, source_id INTEGER, kind TEXT, new INTEGER, changed INTEGER,
    removed INTEGER, errors INTEGER, secs REAL);
"""


class Refused(Exception):
    """An action was refused for a reason worth showing to the user."""


# ---------------------------------------------------------------- jobs (progress)
class Job:
    def __init__(self):
        self.id = secrets.token_hex(8)
        self.state = "running"   # running | done | error
        self.phase = "Preparazione…"
        self.done = 0
        self.total = 0
        self.result = None
        self.error = None

    def update(self, phase=None, done=None, total=None):
        if phase is not None:
            self.phase = phase
        if done is not None:
            self.done = done
        if total is not None:
            self.total = total

    def advance(self, n):
        self.done += n

    def view(self):
        return dict(state=self.state, phase=self.phase, done=self.done, total=self.total,
                    result=self.result, error=self.error)


JOBS = {}
JOBS_LOCK = threading.Lock()
STARTUP_JOB = [None]


def start_job(fn, *args):
    with JOBS_LOCK:
        if any(j.state == "running" for j in JOBS.values()):
            raise Refused("un'altra operazione è ancora in corso: aspetta che finisca")
        job = Job()
        JOBS[job.id] = job
        for old in list(JOBS)[:-8]:
            if old != STARTUP_JOB[0]:
                JOBS.pop(old, None)

    def run():
        with LOCK:
            try:
                job.result = fn(*args, job)
                job.state = "done"
            except Refused as e:
                job.error, job.state = str(e), "error"
            except Exception as e:  # never leave the page waiting forever
                job.error, job.state = f"{type(e).__name__}: {e}", "error"
            GEN[0] += 1

    threading.Thread(target=run, daemon=True).start()
    return job


# ---------------------------------------------------------------- helpers
VERSION = SCAN.program_version()


def connect(db):
    con = sqlite3.connect(db, isolation_level=None, timeout=30)
    con.executescript(SCAN.DDL)
    SCAN.check_schema(con)
    con.executescript(DDL)
    return con


def connect_ro(db):
    return sqlite3.connect(f"file:{Path(db).resolve().as_posix()}?mode=ro", uri=True, timeout=30)


def log(action, source, path, nfiles, nbytes):
    f = HERE / "log.csv"
    new = not f.exists()
    with open(f, "a", newline="", encoding="utf-8") as fh:
        w = csv.writer(fh)
        if new:
            w.writerow(["time", "action", "source", "path", "files", "bytes"])
        w.writerow([time.strftime("%Y-%m-%d %H:%M:%S"), action, source, path, nfiles, nbytes])


def safe_label(label):
    return re.sub(r"[^\w .-]", "_", label).strip() or "source"


def clean_rel(rel):
    rel = rel.replace("\\", "/").strip("/")
    parts = rel.split("/")
    if not rel or any(p in ("", ".", "..") for p in parts) or ".sidecar" in parts or ":" in parts[0]:
        raise Refused(f"percorso non valido: {rel!r}")
    return rel


def inside(root, full):
    root_r, full_r = os.path.realpath(root), os.path.realpath(full)
    return os.path.commonpath([root_r, full_r]) == root_r and full_r != root_r


def resolve_root(con, sid, root, disk_root):
    """Where is this source mounted on THIS computer?

    The index remembers the path used at scan time (e.g. /Volumes/MyDisk); on another computer
    the same disk has another path (E:\\). If the stored root is missing, check that a few
    indexed files exist (same size) under the disk that holds this index; if so use that.
    """
    if os.path.isdir(root):
        return root
    if disk_root is None:
        return root
    sample = con.execute("SELECT path, size FROM files WHERE source_id=? ORDER BY random() LIMIT 8", (sid,)).fetchall()
    if not sample:
        return root
    ok = sum(1 for p, z in sample if os.path.isfile(Path(disk_root) / p) and os.path.getsize(Path(disk_root) / p) == z)
    return str(disk_root) if ok >= max(1, len(sample) - 1) else root


# ---------------------------------------------------------------- disks and sources
class Disk:
    def __init__(self, root_dir, db):
        self.root_dir = Path(root_dir) if root_dir else None
        self.db = str(db)
        self.key = hashlib.sha1(self.db.encode()).hexdigest()[:8]


class Src:
    def __init__(self, label, raw_label, db, sid, root, kind, scanned_at, disk):
        self.label, self.raw_label, self.db, self.sid, self.root = label, raw_label, db, sid, root
        self.kind, self.scanned_at, self.disk = kind, scanned_at, disk


def mount_candidates():
    out = []
    if os.name == "nt":
        out = [Path(f"{d}:\\") for d in string.ascii_uppercase if os.path.exists(f"{d}:\\")]
    else:
        bases = ([] if NO_DEFAULT_MOUNTS[0] else ["/Volumes", "/media", "/mnt", "/run/media"]) + EXTRA_MOUNTS
        for base in bases:
            try:
                for p in sorted(Path(base).iterdir()):
                    out.append(p)
                    if base == "/run/media" and p.is_dir():   # /run/media/<user>/<disk>
                        out.extend(sorted(q for q in p.iterdir() if q.is_dir()))
            except OSError:
                pass
    return out


def discover():
    """All disks we can see that carry an index, the home disk first."""
    disks, seen = [], set()

    def add(root_dir, db):
        try:
            real = os.path.realpath(db)
        except OSError:
            return
        if real in seen or not os.path.isfile(db):
            return
        seen.add(real)
        disks.append(Disk(root_dir, db))

    if HOME_DB[0]:
        add(Path(HOME_DB[0]).parent, HOME_DB[0])
    elif HERE.name == ".sidecar":
        add(HERE.parent, HERE / "index.db")
    for p in mount_candidates():
        try:
            add(p, p / ".sidecar" / "index.db")
        except OSError:
            pass
    return disks


def peer_dbs(disks):
    out, seen = [], set()
    for d in disks:
        if not d.root_dir:
            continue
        try:
            for f in sorted((d.root_dir / ".sidecar" / "peers").glob("*.db")):
                if str(f.resolve()) not in seen:
                    seen.add(str(f.resolve()))
                    out.append((d, str(f)))
        except OSError:
            pass
    return out


def list_sources():
    """Every source visible now, with a label that is unique across disks and snapshots."""
    srcs, used = [], set()
    disks = discover()

    def uniq(label, hint):
        lab = label
        if lab in used:
            lab = f"{label} ({hint})"
        while lab in used:
            lab += "'"
        used.add(lab)
        return lab

    for d in disks:
        try:
            con = connect_ro(d.db)
            rows = con.execute("SELECT id, label, root, scanned_at FROM sources ORDER BY id").fetchall()
            for sid, label, root, at in rows:
                real = resolve_root(con, sid, root, d.root_dir)
                hint = d.root_dir.name if d.root_dir else "disco"
                srcs.append(Src(uniq(label, hint), label, d.db, sid, real, "disk", at, d))
            con.close()
        except sqlite3.Error:
            continue
    for d, f in peer_dbs(disks):
        try:
            con = connect_ro(f)
            for sid, label, root, at in con.execute("SELECT id, label, root, scanned_at FROM sources ORDER BY id"):
                srcs.append(Src(uniq(label, "istantanea"), label, f, sid, root, "snapshot", at, None))
            con.close()
        except sqlite3.Error:
            continue
    return srcs, disks


def disk_id_of(db):
    try:
        con = connect_ro(db)
        r = con.execute("SELECT value FROM meta WHERE key='disk_id'").fetchone()
        con.close()
        return r[0] if r else None
    except sqlite3.Error:
        return None


# ---------------------------------------------------------------- index served to the page
CACHE = {"sig": None, "path": None}


def snapshot(db, dest):
    src = connect_ro(db)
    dst = sqlite3.connect(dest)
    src.backup(dst)
    dst.close()
    src.close()


def build_combined(srcs, dest):
    if os.path.exists(dest):
        os.remove(dest)
    con = sqlite3.connect(dest, uri=True, isolation_level=None)
    con.executescript(SCAN.DDL)
    con.execute("ALTER TABLE sources ADD COLUMN kind TEXT")
    con.execute("ALTER TABLE sources ADD COLUMN disk_id TEXT")
    attached = {}
    for s in srcs:   # ATTACH cannot happen inside a transaction
        if s.db not in attached:
            alias = f"d{len(attached)}"
            con.execute(f"ATTACH DATABASE ? AS {alias}", (f"file:{Path(s.db).resolve().as_posix()}?mode=ro",))
            attached[s.db] = alias
    con.execute("BEGIN")
    for s in srcs:
        a = attached[s.db]
        new = con.execute("INSERT INTO sources(label, root, host, scanned_at, kind, disk_id) VALUES (?,?,?,?,?,?)",
                          (s.label, s.root, "", s.scanned_at, s.kind, disk_id_of(s.db))).lastrowid
        con.execute(f"INSERT INTO files(source_id, path, name, ext, size, mtime, kind, top, sha256, seen) "
                    f"SELECT ?, path, name, ext, size, mtime, kind, top, sha256, 1 FROM {a}.files WHERE source_id=?",
                    (new, s.sid))
        try:
            con.execute(f"INSERT INTO file_text(rowid, path, body) SELECT m.id, t.path, t.body FROM {a}.file_text t "
                        f"JOIN {a}.files sf ON sf.id = t.rowid JOIN files m ON m.source_id = ? AND m.path = sf.path "
                        f"WHERE sf.source_id = ?", (new, s.sid))
            con.execute(f"INSERT OR IGNORE INTO marks SELECT ?, path, mark, note FROM {a}.marks WHERE source_id=?",
                        (new, s.sid))
        except sqlite3.OperationalError:
            pass
    con.execute("COMMIT")
    con.close()


def index_file():
    """Path of the index to hand to the page: one disk as it is, or all of them combined."""
    with INDEX_LOCK:
        srcs, disks = list_sources()
        dbs = sorted({s.db for s in srcs})
        if not dbs:
            return None
        sig = (GEN[0], tuple((p, os.stat(p).st_mtime_ns, os.stat(p).st_size) for p in dbs))
        if CACHE["sig"] == sig and CACHE["path"] and os.path.exists(CACHE["path"]):
            return CACHE["path"]
        tmp = Path(tempfile.gettempdir()) / f"sidecar-{os.getpid()}"
        tmp.mkdir(exist_ok=True)
        dest = str(tmp / f"index-{GEN[0]}-{int(time.time())}.db")
        if len(dbs) == 1:
            snapshot(dbs[0], dest)
        else:
            build_combined(srcs, dest)
        old = CACHE["path"]
        CACHE.update(sig=sig, path=dest)
        if old and os.path.exists(old):
            try:
                os.remove(old)
            except OSError:
                pass
        return dest


# ---------------------------------------------------------------- marks
def do_marks(items):
    srcs = {s.label: s for s in list_sources()[0]}
    groups = {}
    for it in items:
        s = srcs.get(it.get("source"))
        if not s:
            raise Refused(f"sorgente sconosciuta: {it.get('source')}")
        if s.kind != "disk":
            raise Refused(f"{s.label} è un'istantanea di sola lettura: i segni vanno messi dal suo computer")
        rel = clean_rel(it.get("path", ""))
        mark = it.get("mark")
        if mark not in (None, "keep", "delete", "review"):
            raise Refused(f"segno non valido: {mark!r}")
        groups.setdefault(s.db, []).append((s.sid, rel, mark, it.get("note")))
    for db, rows in groups.items():
        con = connect(db)
        con.execute("BEGIN")
        for sid, rel, mark, note in rows:
            if mark is None:
                con.execute("DELETE FROM marks WHERE source_id=? AND path=?", (sid, rel))
            else:
                con.execute("INSERT INTO marks(source_id, path, mark, note) VALUES (?,?,?,?) "
                            "ON CONFLICT(source_id, path) DO UPDATE SET mark=excluded.mark, note=excluded.note",
                            (sid, rel, mark, note))
        con.execute("COMMIT")
        con.close()
    GEN[0] += 1
    return {"saved": len(items)}


# ---------------------------------------------------------------- quarantine
def plan(con, items, job, disk_root):
    """Validate items of ONE disk index (raw source labels); raises Refused with all problems."""
    srcs = {label: (sid, resolve_root(con, sid, root, disk_root))
            for sid, label, root in con.execute("SELECT id, label, root FROM sources")}
    problems, moves = [], []
    for n, it in enumerate(items, 1):
        label, rel = it["source"], it["path"]
        if job:
            job.update(f"Verifico {n}/{len(items)}: {rel.split('/')[-1]}", 0, 0)
        try:
            sid, root = srcs[label]
            if not os.path.isdir(root):
                raise Refused(f"{label}: il disco non è raggiungibile da questo computer ({root})")
            full = os.path.join(root, *rel.split("/"))
            if not inside(root, full):
                raise Refused(f"{rel}: fuori dalla sorgente")
            if os.path.islink(full):
                raise Refused(f"{rel}: è un collegamento simbolico")
            if os.path.isfile(full):
                rows = con.execute("SELECT id, path, size, mtime, sha256 FROM files WHERE source_id=? AND path=?",
                                   (sid, rel)).fetchall()
                if not rows:
                    raise Refused(f"{rel}: non è nell'indice (rifai la scansione)")
                st = os.stat(full)
                if st.st_size != rows[0][2] or int(st.st_mtime) != rows[0][3]:
                    raise Refused(f"{rel}: modificato dopo la scansione (rifai la scansione)")
                is_dir = 0
            elif os.path.isdir(full):
                pre = rel + "/"
                rows = con.execute("SELECT id, path, size, mtime, sha256 FROM files WHERE source_id=? "
                                   "AND substr(path,1,?)=?", (sid, len(pre), pre)).fetchall()
                known = {r[1]: (r[2], r[3]) for r in rows}
                for dp, dn, fn in os.walk(full):
                    dn[:] = [d for d in dn if d not in SCAN.SKIP_DIRS]
                    for nme in fn:
                        if nme in SCAN.SKIP_FILES or nme.startswith("._"):
                            continue
                        p = os.path.relpath(os.path.join(dp, nme), root).replace(os.sep, "/")
                        st = os.stat(os.path.join(dp, nme))
                        if known.get(p) != (st.st_size, int(st.st_mtime)):
                            raise Refused(f"{rel}: la cartella è cambiata dopo la scansione ({p}); rifai la scansione")
                is_dir = 1
            else:
                raise Refused(f"{rel}: non esiste")
            moves.append(dict(label=label, sid=sid, root=root, rel=rel, full=full, is_dir=is_dir, rows=rows))
        except Refused as e:
            problems.append(str(e))
    if problems:
        raise Refused("\n".join(problems))
    keep = []   # drop items nested inside another selected item
    for m in moves:
        nested = any(o is not m and o["sid"] == m["sid"] and o["is_dir"] and m["rel"].startswith(o["rel"] + "/")
                     for o in moves)
        if not nested and not any(k["sid"] == m["sid"] and k["rel"] == m["rel"] for k in keep):
            keep.append(m)
    return keep


def check_last_copy(live_dbs, all_moves):
    """Refuse if the move would remove every remaining copy of a duplicated file, on any mounted disk."""
    moving = {}
    for m in all_moves:
        for r in m["rows"]:
            if r[4]:
                moving[r[4]] = moving.get(r[4], 0) + 1
    if not moving:
        return
    total = {}
    shas = list(moving)
    for db in live_dbs:
        con = connect_ro(db)
        for i in range(0, len(shas), 500):
            chunk = shas[i:i + 500]
            for sha, n in con.execute(f"SELECT sha256, COUNT(*) FROM files WHERE sha256 IN ({','.join('?' * len(chunk))}) "
                                      f"GROUP BY sha256", chunk):
                total[sha] = total.get(sha, 0) + n
        con.close()
    last = [sha for sha, n in moving.items() if total.get(sha, 0) >= 2 and total[sha] == n]
    if last:
        ex = None
        for db in live_dbs:
            con = connect_ro(db)
            r = con.execute("SELECT path FROM files WHERE sha256=? LIMIT 1", (last[0],)).fetchone()
            con.close()
            if r:
                ex = r[0]
                break
        raise Refused(f"Rifiutato: verrebbero spostate TUTTE le copie di {len(last)} file duplicati "
                      f"(es. {ex}). Tieni almeno una copia per gruppo.")


def do_quarantine(items, job=None):
    srcs, disks = list_sources()
    by_label = {s.label: s for s in srcs}
    by_db = {}
    for it in items:
        s = by_label.get(it.get("source"))
        if not s:
            raise Refused(f"sorgente sconosciuta: {it.get('source')}")
        if s.kind != "disk":
            raise Refused(f"{s.label} è un'istantanea di sola lettura: non posso spostare i suoi file")
        by_db.setdefault(s.db, (s.disk, []))[1].append({"source": s.raw_label, "path": clean_rel(it.get("path", ""))})
    plans = []
    for db, (disk, its) in by_db.items():
        con = connect(db)
        for m in plan(con, its, job, disk.root_dir):
            m.update(con=con, db=db)
            plans.append(m)
    if not plans:
        raise Refused("niente da spostare")
    check_last_copy([d.db for d in disks], plans)
    if job:
        job.update("Sposto in quarantena…", 0, sum(len(m["rows"]) for m in plans))
    batch = time.strftime("%Y-%m-%d_%H%M%S")
    done, nfiles, nbytes = [], 0, 0
    backed = set()
    for m in plans:
        if m["db"] not in backed:
            shutil.copy2(m["db"], m["db"] + ".bak")
            backed.add(m["db"])
        con = m["con"]
        qrel = f"{QDIR}/{batch}/{safe_label(m['label'])}/{m['rel']}"
        qfull = os.path.join(m["root"], *qrel.split("/"))
        os.makedirs(os.path.dirname(qfull), exist_ok=True)
        size = sum(r[2] for r in m["rows"])
        try:
            os.rename(m["full"], qfull)
        except OSError as e:
            raise Refused(f"{m['rel']}: spostamento non riuscito ({e}). Spostati finora: {len(done)} elementi.")
        con.execute("BEGIN")
        mid = con.execute("INSERT INTO quarantine_moves(batch, source_id, path, qpath, is_dir, nfiles, size, moved_at) "
                          "VALUES (?,?,?,?,?,?,?,?)",
                          (batch, m["sid"], m["rel"], qrel, m["is_dir"], len(m["rows"]), size, int(time.time()))).lastrowid
        ids = [r[0] for r in m["rows"]]
        for i in range(0, len(ids), 500):
            chunk = ",".join("?" * len(ids[i:i + 500]))
            con.execute(f"INSERT INTO quarantine_files SELECT ?, path, name, ext, size, mtime, kind, top, sha256 "
                        f"FROM files WHERE id IN ({chunk})", [mid] + ids[i:i + 500])
            con.execute(f"DELETE FROM file_text WHERE rowid IN ({chunk})", ids[i:i + 500])
            con.execute(f"DELETE FROM files WHERE id IN ({chunk})", ids[i:i + 500])
            if job:
                job.advance(len(ids[i:i + 500]))
        con.execute("DELETE FROM marks WHERE source_id=? AND (path=? OR substr(path,1,?)=?)",
                    (m["sid"], m["rel"], len(m["rel"]) + 1, m["rel"] + "/"))
        con.execute("COMMIT")
        log("quarantine", m["label"], m["rel"], len(m["rows"]), size)
        done.append(m["rel"])
        nfiles += len(m["rows"])
        nbytes += size
    for m in plans:
        m["con"].close()
    return {"moved": len(done), "files": nfiles, "bytes": nbytes, "batch": batch}


def quarantine_state():
    srcs, disks = list_sources()
    moves = []
    for d in disks:
        con = connect_ro(d.db)   # read only: never wait for a running scan
        labels = {s.sid: s.label for s in srcs if s.db == d.db}
        try:
            rows = con.execute("SELECT id, batch, source_id, path, is_dir, nfiles, size, moved_at FROM quarantine_moves "
                               "WHERE state='moved' ORDER BY id DESC").fetchall()
        except sqlite3.OperationalError:   # no quarantine yet on this disk
            rows = []
        for i, b, sid, p, isd, n, z, t in rows:
            moves.append(dict(id=f"{d.key}:{i}", batch=b, source=labels.get(sid, str(sid)), path=p, is_dir=isd,
                              files=n, size=z, moved_at=t))
        con.close()
    moves.sort(key=lambda m: -m["moved_at"])
    return moves


def do_restore(move_id):
    key, _, num = str(move_id).partition(":")
    srcs, disks = list_sources()
    disk = next((d for d in disks if d.key == key), None)
    if not disk:
        raise Refused("il disco di questo elemento non è collegato")
    con = connect(disk.db)
    row = con.execute("SELECT id, source_id, path, qpath, is_dir, size FROM quarantine_moves "
                      "WHERE id=? AND state='moved'", (int(num),)).fetchone()
    if not row:
        raise Refused("elemento non trovato in quarantena")
    mid, sid, rel, qrel, is_dir, size = row
    src = next(s for s in srcs if s.db == disk.db and s.sid == sid)
    root = src.root
    full, qfull = os.path.join(root, *rel.split("/")), os.path.join(root, *qrel.split("/"))
    if not os.path.isdir(root):
        raise Refused("il disco non è raggiungibile")
    if os.path.exists(full):
        raise Refused(f"{rel}: esiste già un file o cartella con quel nome; non sovrascrivo")
    if not os.path.exists(qfull):
        raise Refused(f"{rel}: non c'è più nella cartella di quarantena")
    os.makedirs(os.path.dirname(full), exist_ok=True)
    os.rename(qfull, full)
    con.execute("BEGIN")
    con.execute("INSERT OR IGNORE INTO files(source_id, path, name, ext, size, mtime, kind, top, sha256, seen) "
                "SELECT ?, path, name, ext, size, mtime, kind, top, sha256, 1 FROM quarantine_files WHERE move_id=?",
                (sid, mid))
    con.execute("DELETE FROM quarantine_files WHERE move_id=?", (mid,))
    con.execute("UPDATE quarantine_moves SET state='restored' WHERE id=?", (mid,))
    con.execute("COMMIT")
    log("restore", src.label, rel, 0, size)
    con.close()
    GEN[0] += 1
    return {"restored": rel, "note": "Rifai la verifica per ripristinare anche il testo ricercabile."}


def rm_tree(path, job):
    """Delete a file or folder, counting files for the progress bar."""
    if os.path.isdir(path):
        for dp, dn, fn in os.walk(path, topdown=False):
            for n in fn:
                os.remove(os.path.join(dp, n))
                if job:
                    job.advance(1)
            for d in dn:
                os.rmdir(os.path.join(dp, d))
        os.rmdir(path)
    elif os.path.exists(path):
        os.remove(path)
        if job:
            job.advance(1)


def do_empty(confirm, job=None):
    if confirm != "ELIMINA":
        raise Refused("conferma mancante")
    srcs, disks = list_sources()
    cons = [(d, connect(d.db)) for d in disks]
    if job:
        job.update("Elimino definitivamente…", 0,
                   sum(c.execute("SELECT COALESCE(SUM(nfiles),0) FROM quarantine_moves WHERE state='moved'").fetchone()[0]
                       for _, c in cons))
    freed, count = 0, 0
    for d, con in cons:
        roots = {}
        for s in srcs:
            if s.db == d.db:
                roots[s.sid] = (s.label, s.root)
        for mid, qrel, size, sid in con.execute("SELECT id, qpath, size, source_id FROM quarantine_moves "
                                                "WHERE state='moved'").fetchall():
            label, root = roots[sid]
            if not os.path.isdir(root):
                continue
            rm_tree(os.path.join(root, *qrel.split("/")), job)
            con.execute("DELETE FROM quarantine_files WHERE move_id=?", (mid,))
            con.execute("UPDATE quarantine_moves SET state='deleted' WHERE id=?", (mid,))
            log("delete", label, qrel, 0, size)
            freed += size
            count += 1
        for root in {r for _, r in roots.values()}:   # remove emptied batch folders
            base = os.path.join(root, *QDIR.split("/"))
            if os.path.isdir(base):
                for dp, _, _ in os.walk(base, topdown=False):
                    try:
                        os.rmdir(dp)
                    except OSError:
                        pass
        con.close()
    return {"deleted": count, "bytes": freed}


# ---------------------------------------------------------------- verification of changes made by hand
LAST_VERIFY = [None]


def do_verify(job=None):
    srcs, disks = list_sources()
    totals = dict(new=0, changed=0, removed=0, errors=0, disks=0, secs=0.0, at=int(time.time()), details=[])
    host = platform.node()
    for s in srcs:
        if s.kind != "disk" or not os.path.isdir(s.root):
            continue
        con = connect_ro(s.db)
        hint = con.execute("SELECT COUNT(*) FROM files WHERE source_id=?", (s.sid,)).fetchone()[0]
        con.close()
        if job:
            job.update(f"Verifico {s.label}…", 0, hint)
        st = SCAN.scan(s.root, s.db, s.raw_label, host, None, True,
                       progress=(lambda n, j=job: j and j.update(done=n)), quiet=True)
        con = connect(s.db)
        con.execute("INSERT INTO scan_log VALUES (?,?,?,?,?,?,?,?)",
                    (int(time.time()), s.sid, "verify", st["new"], st["changed"], st["removed"], st["errors"], st["secs"]))
        con.close()
        for k in ("new", "changed", "removed", "errors"):
            totals[k] += st[k]
        totals["secs"] += st["secs"]
        totals["disks"] += 1
        if st["new"] or st["changed"] or st["removed"]:
            totals["details"].append(dict(source=s.label, new=st["new"], changed=st["changed"], removed=st["removed"]))
    LAST_VERIFY[0] = totals
    return totals


# ---------------------------------------------------------------- state for the page
def state():
    srcs, disks = list_sources()
    warnings, ids = [], {}
    for d in disks:
        did = disk_id_of(d.db)
        if did in ids:
            warnings.append(f"Due dischi hanno lo stesso ID ({ids[did]} e {d.root_dir}): uno è un clone dell'altro.")
        ids[did] = d.root_dir
    return {"actions": True, "version": VERSION,
            "quarantine": quarantine_state(),
            "sources": [dict(label=s.label, kind=s.kind, reachable=(s.kind == "disk" and os.path.isdir(s.root)),
                             scanned_at=s.scanned_at) for s in srcs],
            "startup_job": STARTUP_JOB[0],
            "verify": LAST_VERIFY[0],
            "warnings": warnings}


# ---------------------------------------------------------------- HTTP
def make_handler(token, port):
    class Handler(http.server.SimpleHTTPRequestHandler):
        def __init__(self, *a, **kw):
            super().__init__(*a, directory=str(HERE), **kw)

        def log_message(self, fmt, *args):
            pass

        def _host_ok(self):
            return self.headers.get("Host", "") in (f"127.0.0.1:{port}", f"localhost:{port}")

        def _json(self, code, obj):
            body = json.dumps(obj, ensure_ascii=False).encode()
            self.send_response(code)
            self.send_header("Content-Type", "application/json; charset=utf-8")
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            self.wfile.write(body)

        def _authorised(self):
            return self._host_ok() and secrets.compare_digest(self.headers.get("X-Sidecar-Token", ""), token)

        def _file(self, path, ctype):
            size = os.path.getsize(path)
            self.send_response(200)
            self.send_header("Content-Type", ctype)
            self.send_header("Content-Length", str(size))
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            with open(path, "rb") as f:
                while True:
                    chunk = f.read(1 << 16)   # small writes: macOS answers ENOBUFS to big ones
                    if not chunk:
                        break
                    for attempt in range(200):
                        try:
                            self.wfile.write(chunk)
                            break
                        except OSError as e:
                            if e.errno not in (55, 105) or attempt == 199:   # ENOBUFS: wait and retry
                                raise
                            time.sleep(0.01)

        def do_GET(self):
            if not self._host_ok():
                return self._json(403, {"error": "host non consentito"})
            path = self.path.split("?")[0]
            if path == "/api/job":
                if not self._authorised():
                    return self._json(403, {"error": "token mancante"})
                jid = urllib.parse.parse_qs(urllib.parse.urlparse(self.path).query).get("id", [""])[0]
                job = JOBS.get(jid)
                return self._json(200, job.view()) if job else self._json(404, {"error": "operazione sconosciuta"})
            if path == "/api/state":
                if not self._authorised():
                    return self._json(403, {"error": "token mancante"})
                return self._json(200, state())
            if path in ("/", "/catalog.html"):
                html = (HERE / "catalog.html").read_text(encoding="utf-8").replace(
                    '<meta name="sidecar-token" content="">', f'<meta name="sidecar-token" content="{token}">')
                body = html.encode()
                self.send_response(200)
                self.send_header("Content-Type", "text/html; charset=utf-8")
                self.send_header("Content-Length", str(len(body)))
                self.send_header("Cache-Control", "no-store")
                self.end_headers()
                return self.wfile.write(body)
            if path == "/index.db":
                f = index_file()
                if not f:
                    return self.send_error(404, "nessun indice trovato")
                return self._file(f, "application/octet-stream")
            if path.startswith("/vendor/"):
                clean = posixpath.normpath(urllib.parse.unquote(path))   # no way out of vendor/ with .. or %2e%2e
                if not clean.startswith("/vendor/") or ".." in clean.split("/"):
                    return self.send_error(404)
                return super().do_GET()
            self.send_error(404)

        def do_POST(self):
            if not self._authorised():
                return self._json(403, {"error": "token mancante o host non consentito"})
            n = int(self.headers.get("Content-Length", 0))
            if n > 20_000_000:
                return self._json(413, {"error": "richiesta troppo grande"})
            try:
                data = json.loads(self.rfile.read(n) or b"{}")
                if self.path == "/api/marks":
                    with LOCK:
                        return self._json(200, do_marks(data.get("items", [])))
                if self.path == "/api/restore":
                    with LOCK:
                        return self._json(200, do_restore(data["id"]))
                if self.path == "/api/quarantine":
                    job = start_job(do_quarantine, data.get("items", []))
                elif self.path == "/api/empty":
                    if data.get("confirm") != "ELIMINA":
                        raise Refused("conferma mancante")
                    job = start_job(do_empty, data.get("confirm"))
                elif self.path == "/api/verify":
                    job = start_job(do_verify)
                else:
                    return self._json(404, {"error": "azione sconosciuta"})
                self._json(202, {"job": job.id})
            except Refused as e:
                self._json(409, {"error": str(e)})
            except (KeyError, ValueError, TypeError, StopIteration) as e:
                self._json(400, {"error": f"richiesta non valida: {e}"})

    return Handler


class Server(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True
    allow_reuse_address = True


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--version", action="version", version=f"disk-sidecar {VERSION}")
    ap.add_argument("--port", type=int, default=8765)
    ap.add_argument("--db", help="use this index as the home disk (development)")
    ap.add_argument("--mounts", action="append", default=[], help="extra folder that contains mounted disks")
    ap.add_argument("--no-default-mounts", action="store_true", help="do not look in /Volumes, /media, /mnt (testing)")
    ap.add_argument("--no-open", action="store_true")
    ap.add_argument("--no-verify", action="store_true", help="do not re-check the disks at start")
    a = ap.parse_args()
    EXTRA_MOUNTS.extend(a.mounts)
    NO_DEFAULT_MOUNTS[0] = a.no_default_mounts
    HOME_DB[0] = a.db
    token = secrets.token_urlsafe(24)
    srv = Server(("127.0.0.1", a.port), make_handler(token, a.port))
    url = f"http://127.0.0.1:{a.port}/catalog.html"
    srcs, disks = list_sources()
    print(f"Disk Sidecar: {url}")
    for d in disks:
        print(f"  disco: {d.root_dir}  ({sum(1 for s in srcs if s.db == d.db)} sorgenti)")
    if not disks:
        print("  nessun indice trovato: lancia prima scan.py")
    print("Ctrl+C per fermare.")
    if disks and not a.no_verify:
        STARTUP_JOB[0] = start_job(do_verify).id
    if not a.no_open:
        threading.Timer(0.8, lambda: webbrowser.open(url)).start()
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
