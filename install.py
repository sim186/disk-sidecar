#!/usr/bin/env python3
"""Install or update Disk Sidecar on a disk.

    python3 install.py /Volumes/MyDisk                  # install, or update an existing sidecar
    python3 install.py E:\\ --label "My disk" --scan     # Windows; also scan the disk right away

It copies the programs into <disk>/.sidecar/. An update never touches your data there:
index.db (and its backup), log.csv, peers/ and _quarantena/ are left alone.
Needs only Python 3.9 or newer; no packages.
"""
import argparse
import os
import shutil
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
SRC = os.path.join(HERE, "sidecar")
KEEP = {"index.db", "index.db.bak", "log.csv", "peers", "_quarantena", "MANIFEST"}   # user data, never overwritten
SKIP_DIRS = {"__pycache__"}


def read(path):
    try:
        with open(path, encoding="utf-8") as f:
            return f.read().strip()
    except OSError:
        return None


def package_files():
    out = []
    for dp, dn, fn in os.walk(SRC):
        dn[:] = [d for d in dn if d not in SKIP_DIRS]
        for n in fn:
            if n.startswith("._") or n == ".DS_Store" or n.endswith(".pyc"):
                continue
            out.append(os.path.relpath(os.path.join(dp, n), SRC).replace(os.sep, "/"))
    return sorted(out)


def same(a, b):
    try:
        if os.path.getsize(a) != os.path.getsize(b):
            return False
        with open(a, "rb") as x, open(b, "rb") as y:
            return x.read() == y.read()
    except OSError:
        return False


def clean_metadata_files(target):
    """macOS leaves ._name files on exFAT/NTFS for files it tags; they are noise inside .sidecar."""
    for dp, dn, fn in os.walk(target):
        for n in fn:
            if n.startswith("._"):
                try:
                    os.remove(os.path.join(dp, n))
                except OSError:
                    pass


def ask(question, default_yes=True):
    if not sys.stdin.isatty():
        return default_yes
    a = input(f"{question} [{'Y/n' if default_yes else 'y/N'}] ").strip().lower()
    return default_yes if not a else a.startswith("y") or a.startswith("s")


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("disk", help="root folder of the disk (e.g. /Volumes/MyDisk or E:\\)")
    ap.add_argument("--label", help="name of the disk shown in the app (default: the folder name)")
    ap.add_argument("--scan", action="store_true", help="scan the disk after installing, without asking")
    ap.add_argument("--no-scan", action="store_true", help="do not offer to scan")
    ap.add_argument("--yes", action="store_true", help="do not ask questions")
    ap.add_argument("--dry-run", action="store_true", help="show what would be done")
    a = ap.parse_args()

    if sys.version_info < (3, 9):
        sys.exit(f"Python 3.9 or newer is needed (this is {sys.version.split()[0]}).")
    if not os.path.isdir(SRC):
        sys.exit(f"Cannot find the 'sidecar' folder next to install.py ({SRC}).")
    disk = os.path.abspath(a.disk)
    if not os.path.isdir(disk):
        sys.exit(f"Not a folder: {disk}. Is the disk plugged in?")
    if not os.path.ismount(disk) and not a.yes and not a.dry_run:
        if not ask(f"{disk} is not the root of a disk. Sidecar will live in {disk}{os.sep}.sidecar. Continue?", False):
            sys.exit("Cancelled.")

    target = os.path.join(disk, ".sidecar")
    new_v, old_v = read(os.path.join(SRC, "VERSION")), read(os.path.join(target, "VERSION"))
    files = package_files()
    old_manifest = (read(os.path.join(target, "MANIFEST")) or "").splitlines()
    print(f"Disk Sidecar {new_v} -> {target}" + (f"  (replacing {old_v})" if old_v else "  (new install)"))
    if old_v and old_v == new_v:
        print("Same version already installed: checking the files.")

    changed = 0
    for rel in files:
        src, dst = os.path.join(SRC, *rel.split("/")), os.path.join(target, *rel.split("/"))
        if rel.split("/")[0] in KEEP:
            continue
        if same(src, dst):
            continue
        changed += 1
        print(("would copy  " if a.dry_run else "copy  ") + rel)
        if not a.dry_run:
            os.makedirs(os.path.dirname(dst), exist_ok=True)
            shutil.copyfile(src, dst)          # copyfile: no extended attributes, so no ._ files on exFAT
            if rel.endswith(".command") or rel.endswith(".py"):
                os.chmod(dst, 0o755)
    removed = [r for r in old_manifest if r and r not in files and r.split("/")[0] not in KEEP]
    for rel in removed:
        print(("would remove  " if a.dry_run else "remove  ") + rel)
        if not a.dry_run:
            try:
                os.remove(os.path.join(target, *rel.split("/")))
            except OSError:
                pass
    if not a.dry_run:
        with open(os.path.join(target, "MANIFEST"), "w", encoding="utf-8") as f:
            f.write("\n".join(files) + "\n")
        if os.name == "nt":
            subprocess.run(["attrib", "+h", target], check=False, capture_output=True)
    print(f"{changed} file(s) {'to copy' if a.dry_run else 'copied'}, {len(removed)} obsolete {'to remove' if a.dry_run else 'removed'}.")
    if a.dry_run:
        return

    label = a.label or os.path.basename(disk.rstrip("\\/")) or disk
    index = os.path.join(target, "index.db")
    if not os.path.exists(index) and not a.no_scan and (a.scan or a.yes or ask(f"Scan the disk now as '{label}'? (about a minute per 100,000 files)")):
        print("Scanning...")
        r = subprocess.run([sys.executable, os.path.join(target, "scan.py"), disk, "--db", index, "--source", label])
        if r.returncode:
            sys.exit("The scan failed; run it again with the command above.")
    elif os.path.exists(index):
        print("An index already exists here: it was kept. The app re-checks the disk when it starts.")
    else:
        print(f"No index yet. Create it with:\n  {sys.executable} \"{os.path.join(target, 'scan.py')}\" \"{disk}\" --db \"{index}\" --source \"{label}\"")
    clean_metadata_files(target)
    launcher = "open-sidecar.bat" if os.name == "nt" else "open-sidecar.command"
    print(f"\nDone. To open the catalog, double click:\n  {os.path.join(target, launcher)}")


if __name__ == "__main__":
    main()
