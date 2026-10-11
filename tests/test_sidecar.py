"""Tests for Disk Sidecar. Standard library only:  python3 -m unittest discover -s tests -v"""
import http.client
import json
import os
import re
import shutil
import socket
import sqlite3
import subprocess
import sys
import tempfile
import time
import unittest
import urllib.error
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SIDECAR = os.path.join(ROOT, "sidecar")
PY = sys.executable


def run(*args, **kw):
    return subprocess.run([PY, *map(str, args)], capture_output=True, text=True, **kw)


def write(path, data):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "wb") as f:
        f.write(data if isinstance(data, bytes) else data.encode())


def blob(n, seed):
    return (str(seed).encode() * (n // len(str(seed)) + 1))[:n]


def read_text(path):
    with open(path, encoding="utf-8") as f:
        return f.read().strip()


def rows(db, sql, *p):
    con = sqlite3.connect(db)
    try:
        return con.execute(sql, p).fetchall()
    finally:
        con.close()


class Base(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="sidecar-test-")
        self.addCleanup(shutil.rmtree, self.tmp, True)

    def p(self, *parts):
        return os.path.join(self.tmp, *parts)


class ScanTests(Base):
    def test_incremental_scan_and_rules(self):
        d = self.p("disk")
        write(f"{d}/a/photo.jpg", blob(2000, 1))
        write(f"{d}/a/notes.txt", "hello needle world")
        write(f"{d}/._photo.jpg", b"appledouble")
        write(f"{d}/.sidecar/index.db", b"must be skipped")
        write(f"{d}/$RECYCLE.BIN/x.bin", b"x")
        db = self.p("i.db")
        r = run(os.path.join(SIDECAR, "scan.py"), d, "--db", db, "--source", "T")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual({r[0] for r in rows(db, "select path from files")}, {"a/photo.jpg", "a/notes.txt"})
        self.assertEqual(rows(db, "select kind from files where name='photo.jpg'"), [("photo",)])
        self.assertTrue(rows(db, "select 1 from file_text where file_text match 'needle'"))
        self.assertTrue(rows(db, "select value from meta where key='disk_id'")[0][0])
        self.assertEqual(rows(db, "select value from meta where key='sidecar_version'")[0][0],
                         read_text(os.path.join(SIDECAR, "VERSION")))
        # second scan: nothing new; then one new, one changed, one removed
        self.assertIn("new 0, changed 0, same 2", run(os.path.join(SIDECAR, "scan.py"), d, "--db", db, "--source", "T").stdout)
        write(f"{d}/a/new.txt", "new")
        time.sleep(1.1)
        write(f"{d}/a/notes.txt", "changed content")
        os.remove(f"{d}/a/photo.jpg")
        out = run(os.path.join(SIDECAR, "scan.py"), d, "--db", db, "--source", "T").stdout
        self.assertIn("new 1, changed 1, same 0, removed 1", out)

    def test_refuses_newer_schema(self):
        d = self.p("disk")
        write(f"{d}/a.txt", "x")
        db = self.p("i.db")
        run(os.path.join(SIDECAR, "scan.py"), d, "--db", db, "--source", "T")
        con = sqlite3.connect(db)
        con.execute("update meta set value='999' where key='schema_version'")
        con.commit(); con.close()
        r = run(os.path.join(SIDECAR, "scan.py"), d, "--db", db, "--source", "T")
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("newer", r.stderr + r.stdout)


class HashAndMergeTests(Base):
    def setUp(self):
        super().setUp()
        self.a, self.b = self.p("A"), self.p("B")
        write(f"{self.a}/x/same.bin", blob(1_200_000, 7))
        write(f"{self.b}/y/copy.bin", blob(1_200_000, 7))
        write(f"{self.a}/x/other.bin", blob(1_300_000, 8))
        write(f"{self.b}/y/diff.bin", blob(1_300_000, 9))   # same size as other.bin, different content
        write(f"{self.a}/x/only.bin", blob(1_100_000, 3))
        self.adb, self.bdb = self.p("a.db"), self.p("b.db")
        run(os.path.join(SIDECAR, "scan.py"), self.a, "--db", self.adb, "--source", "A")
        run(os.path.join(SIDECAR, "scan.py"), self.b, "--db", self.bdb, "--source", "B")

    def test_hash_only_needed_files_and_also_match(self):
        h = os.path.join(SIDECAR, "hash_dups.py")
        self.assertEqual(run(h, "--db", self.adb).returncode, 0)
        self.assertEqual(rows(self.adb, "select count(*) from files where sha256 is not null")[0][0], 0)  # no ties inside A
        run(h, "--db", self.adb, "--also-match", self.bdb)
        run(h, "--db", self.bdb, "--also-match", self.adb)
        hashed = {r[0] for r in rows(self.adb, "select name from files where sha256 is not null")}
        self.assertEqual(hashed, {"same.bin", "other.bin"})       # only.bin has a unique size: never read
        ha = dict(rows(self.adb, "select name, sha256 from files where sha256 is not null"))
        hb = dict(rows(self.bdb, "select name, sha256 from files where sha256 is not null"))
        self.assertEqual(ha["same.bin"], hb["copy.bin"])
        self.assertNotEqual(ha["other.bin"], hb["diff.bin"])

    def test_merge_replaces_same_label(self):
        main = self.p("main.db")
        m = os.path.join(SIDECAR, "merge.py")
        r = run(m, main, self.bdb)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(rows(main, "select count(*) from files")[0][0], 2)
        write(f"{self.b}/y/more.txt", "needle")
        run(os.path.join(SIDECAR, "scan.py"), self.b, "--db", self.bdb, "--source", "B")
        run(m, main, self.bdb)
        self.assertEqual(rows(main, "select count(*) from sources")[0][0], 1)       # replaced, not duplicated
        self.assertEqual(rows(main, "select count(*) from files")[0][0], 3)
        self.assertTrue(rows(main, "select 1 from file_text where file_text match 'needle'"))
        self.assertIn("dry run", run(m, main, self.adb, "--dry-run").stdout)
        self.assertEqual(rows(main, "select count(*) from sources")[0][0], 1)       # dry run changed nothing


class InstallTests(Base):
    def test_install_update_keeps_user_data(self):
        disk = self.p("disk")
        write(f"{disk}/stuff/a.txt", "hi")
        inst = os.path.join(ROOT, "install.py")
        r = run(inst, disk, "--yes", "--label", "Mine")
        self.assertEqual(r.returncode, 0, r.stderr + r.stdout)
        sc = f"{disk}/.sidecar"
        for f in ("catalog.html", "serve.py", "scan.py", "VERSION", "open-sidecar.command", "vendor/sql-wasm.wasm", "index.db"):
            self.assertTrue(os.path.exists(f"{sc}/{f}"), f)
        self.assertEqual(rows(f"{sc}/index.db", "select label from sources"), [("Mine",)])
        # simulate an older install with a file that no longer exists in the package
        write(f"{sc}/VERSION", "0.0.1\n")
        write(f"{sc}/old_tool.py", "x")
        with open(f"{sc}/MANIFEST", "a") as f:
            f.write("old_tool.py\n")
        write(f"{sc}/log.csv", "keep me")
        write(f"{sc}/peers/p.db", b"peer")
        with open(f"{sc}/serve.py", "a") as f:
            f.write("# tampered\n")
        out = run(inst, disk, "--yes").stdout
        self.assertIn("replacing 0.0.1", out)
        self.assertFalse(os.path.exists(f"{sc}/old_tool.py"))
        self.assertEqual(open(f"{sc}/log.csv").read(), "keep me")
        self.assertTrue(os.path.exists(f"{sc}/peers/p.db"))
        self.assertEqual(rows(f"{sc}/index.db", "select count(*) from files")[0][0], 1)
        self.assertNotIn("tampered", open(f"{sc}/serve.py").read())
        self.assertFalse([n for n in os.listdir(sc) if n.startswith("._")])

    def test_dry_run_writes_nothing(self):
        disk = self.p("disk")
        os.makedirs(disk)
        out = run(os.path.join(ROOT, "install.py"), disk, "--dry-run").stdout
        self.assertIn("would copy", out)
        self.assertFalse(os.path.exists(f"{disk}/.sidecar"))


def free_port():
    s = socket.socket(); s.bind(("127.0.0.1", 0)); port = s.getsockname()[1]; s.close()
    return port


class ServerTests(Base):
    """Two plugged-in disks plus a snapshot of a third computer, served together."""

    def setUp(self):
        super().setUp()
        self.vol = self.p("Volumes")
        self.A, self.B = f"{self.vol}/DiskA", f"{self.vol}/DiskB"
        write(f"{self.A}/foto/shared.bin", blob(1_500_000, 1))
        write(f"{self.B}/backup/shared.bin", blob(1_500_000, 1))
        write(f"{self.A}/foto/onlyA.bin", blob(1_200_000, 2))
        write(f"{self.B}/backup/onlyB.bin", blob(1_300_000, 3))
        write(f"{self.A}/doc/note.txt", "ciao")
        write(f"{self.A}/doc/gone.txt", "bye")
        write(f"{self.A}/doc/edit.txt", "v1")
        write(f"{self.tmp}/pc/Foto/shared.bin", blob(1_500_000, 1))
        inst = os.path.join(ROOT, "install.py")
        for d, label in ((self.A, "Disk A"), (self.B, "Disk B")):
            self.assertEqual(run(inst, d, "--yes", "--label", label).returncode, 0)
        h = lambda disk, other: run(f"{disk}/.sidecar/hash_dups.py", "--db", f"{disk}/.sidecar/index.db", "--also-match", f"{other}/.sidecar/index.db")
        h(self.A, self.B); h(self.B, self.A)
        run(os.path.join(SIDECAR, "scan.py"), f"{self.tmp}/pc", "--db", self.p("pc.db"), "--source", "Laptop")
        run(os.path.join(SIDECAR, "hash_dups.py"), "--db", self.p("pc.db"), "--also-match", f"{self.A}/.sidecar/index.db")
        os.makedirs(f"{self.A}/.sidecar/peers")
        shutil.copy(self.p("pc.db"), f"{self.A}/.sidecar/peers/laptop.db")
        # changes made by hand after the scan
        write(f"{self.A}/doc/new.txt", "new")
        os.remove(f"{self.A}/doc/gone.txt")
        time.sleep(1.1)
        write(f"{self.A}/doc/edit.txt", "version two!")
        self.port = free_port()
        self.proc = subprocess.Popen([PY, f"{self.A}/.sidecar/serve.py", "--port", str(self.port), "--no-open",
                                      "--no-default-mounts", "--mounts", self.vol],
                                     stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        self.addCleanup(lambda: (self.proc.terminate(), self.proc.wait(10)))
        self.base = f"http://127.0.0.1:{self.port}"
        for _ in range(100):
            try:
                self.html = urllib.request.urlopen(self.base + "/catalog.html").read().decode()
                break
            except (urllib.error.URLError, ConnectionError):
                time.sleep(0.1)
        else:
            self.fail("server did not start")
        self.token = re.search(r'sidecar-token" content="([^"]+)', self.html).group(1)

    def call(self, path, body=None, token=True, headers=None):
        h = {"Content-Type": "application/json"}
        if token:
            h["X-Sidecar-Token"] = self.token
        h.update(headers or {})
        req = urllib.request.Request(self.base + path, data=None if body is None else json.dumps(body).encode(), headers=h)
        try:
            with urllib.request.urlopen(req) as r:
                return r.status, json.loads(r.read())
        except urllib.error.HTTPError as e:
            with e:
                body = e.read()
                try:
                    return e.code, json.loads(body or b"{}")
                except ValueError:       # e.g. the HTML of a plain 404
                    return e.code, {}

    def job(self, path, body):
        code, j = self.call(path, body)
        if code != 202:
            return "rejected", j.get("error")
        while True:
            _, v = self.call("/api/job?id=" + j["job"])
            if v["state"] != "running":
                return v["state"], (v.get("result") if v["state"] == "done" else v.get("error"))
            time.sleep(0.1)

    def index(self):
        path = self.p("served.db")
        with open(path, "wb") as f:
            f.write(urllib.request.urlopen(self.base + "/index.db").read())
        return path

    def test_disks_found_and_changes_by_hand_detected(self):
        _, st = self.call("/api/state")
        self.assertEqual([(s["label"], s["kind"]) for s in st["sources"]],
                         [("Disk A", "disk"), ("Disk B", "disk"), ("Laptop", "snapshot")])
        while True:
            _, v = self.call("/api/job?id=" + st["startup_job"])
            if v["state"] != "running":
                break
            time.sleep(0.1)
        self.assertEqual((v["result"]["new"], v["result"]["changed"], v["result"]["removed"]), (1, 1, 1))
        db = self.index()
        self.assertEqual(rows(db, "select label, kind from sources order by id"), [("Disk A", "disk"), ("Disk B", "disk"), ("Laptop", "snapshot")])
        self.assertEqual(rows(db, "select count(*) from files where name='gone.txt'")[0][0], 0)
        copies = rows(db, "select count(distinct source_id) from files where sha256 is not null group by sha256 having count(*) > 1")
        self.assertIn((3,), copies)                       # the same file on both disks and on the laptop

    def test_security_rules(self):
        self.assertEqual(self.call("/api/state", token=False)[0], 403)
        self.assertEqual(self.call("/api/state", headers={"Host": "evil.example"})[0], 403)
        self.assertEqual(self.call("/api/marks", {"items": []}, token=False)[0], 403)
        self.assertEqual(self.job("/api/quarantine", {"items": [{"source": "Disk A", "path": "../x"}]})[0], "error")
        self.assertEqual(self.job("/api/quarantine", {"items": [{"source": "Disk A", "path": ".sidecar/index.db"}]})[0], "error")
        self.assertEqual(self.job("/api/quarantine", {"items": [{"source": "Nope", "path": "a"}]})[0], "error")
        self.assertEqual(self.call("/api/empty", {})[0], 409)                      # no confirmation word
        self.assertEqual(self.call("/serve.py")[0], 404)                           # only the app's own files are served
        for raw in ("/vendor/../serve.py", "/vendor/%2e%2e/serve.py", "/vendor/..%2fserve.py"):
            c = http.client.HTTPConnection("127.0.0.1", self.port)
            c.request("GET", raw)                                                  # http.client does not normalise the path
            self.assertEqual(c.getresponse().status, 404, raw)
            c.close()

    def test_marks_follow_the_disk_that_owns_the_file(self):
        self.call("/api/state")
        self.assertEqual(self.call("/api/marks", {"items": [{"source": "Disk B", "path": "backup/onlyB.bin", "mark": "delete"},
                                                              {"source": "Disk A", "path": "foto/onlyA.bin", "mark": "keep"}]})[0], 200)
        self.assertEqual(rows(f"{self.B}/.sidecar/index.db", "select path, mark from marks"), [("backup/onlyB.bin", "delete")])
        self.assertEqual(rows(f"{self.A}/.sidecar/index.db", "select path, mark from marks"), [("foto/onlyA.bin", "keep")])
        code, err = self.call("/api/marks", {"items": [{"source": "Laptop", "path": "Foto/shared.bin", "mark": "delete"}]})
        self.assertEqual(code, 409)
        self.assertIn("sola lettura", err["error"])

    def test_quarantine_restore_empty(self):
        self.call("/api/state")
        time.sleep(0.5)
        # refuses to take every copy of a duplicated file, across two disks
        state, msg = self.job("/api/quarantine", {"items": [{"source": "Disk A", "path": "foto/shared.bin"},
                                                              {"source": "Disk B", "path": "backup/shared.bin"}]})
        self.assertEqual(state, "error")
        self.assertIn("TUTTE le copie", msg)
        self.assertTrue(os.path.exists(f"{self.A}/foto/shared.bin") and os.path.exists(f"{self.B}/backup/shared.bin"))
        # snapshots are read only
        self.assertEqual(self.job("/api/quarantine", {"items": [{"source": "Laptop", "path": "Foto/shared.bin"}]})[0], "error")
        # one copy of the duplicate plus a file with a mark: allowed, marks of moved items disappear
        self.call("/api/marks", {"items": [{"source": "Disk B", "path": "backup/onlyB.bin", "mark": "delete"}]})
        state, res = self.job("/api/quarantine", {"items": [{"source": "Disk B", "path": "backup/shared.bin"},
                                                              {"source": "Disk B", "path": "backup/onlyB.bin"}]})
        self.assertEqual((state, res["moved"]), ("done", 2))
        self.assertFalse(os.path.exists(f"{self.B}/backup/onlyB.bin"))
        self.assertEqual(rows(f"{self.B}/.sidecar/index.db", "select count(*) from marks")[0][0], 0)
        self.assertEqual(rows(f"{self.B}/.sidecar/index.db", "select count(*) from files")[0][0], 0)
        _, st = self.call("/api/state")
        self.assertEqual(len(st["quarantine"]), 2)
        # restore one; it comes back to the same path and the index
        mid = next(m["id"] for m in st["quarantine"] if m["path"] == "backup/onlyB.bin")
        self.assertEqual(self.call("/api/restore", {"id": mid})[0], 200)
        self.assertTrue(os.path.exists(f"{self.B}/backup/onlyB.bin"))
        self.assertEqual(rows(f"{self.B}/.sidecar/index.db", "select count(*) from files")[0][0], 1)
        # empty needs the word; then the rest is really gone
        self.assertEqual(self.job("/api/empty", {"confirm": "ELIMINA"})[0], "done")
        self.assertFalse(os.path.exists(f"{self.B}/.sidecar/_quarantena"))
        self.assertTrue(os.path.exists(f"{self.A}/foto/shared.bin"))

    def test_refuses_changed_files(self):
        self.call("/api/state")
        time.sleep(0.5)
        time.sleep(1.1)
        write(f"{self.A}/foto/onlyA.bin", blob(1_250_000, 4))     # changed after the scan
        state, msg = self.job("/api/quarantine", {"items": [{"source": "Disk A", "path": "foto/onlyA.bin"}]})
        self.assertEqual(state, "error")
        self.assertIn("modificato", msg)
        write(f"{self.A}/doc/extra.txt", "x")                       # folder gained a file
        state, msg = self.job("/api/quarantine", {"items": [{"source": "Disk A", "path": "doc"}]})
        self.assertEqual(state, "error")


if __name__ == "__main__":
    unittest.main()
