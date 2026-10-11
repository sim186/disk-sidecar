# Changelog

All notable changes to Disk Sidecar. Versions follow [Semantic Versioning](https://semver.org/);
the layout of the index has its own number (`schema_version` in the index's `meta` table),
which only changes when older programs could no longer read or write it safely.

## 0.4.1

- Fix: the `Open Sidecar` launchers that 0.4.0 puts in the root of the disk were indexed as if they were
  your files. They are now skipped by the scan. Disks scanned with 0.4.0 lose them from the catalog the
  next time the app checks the disk.

## 0.4.0

- Every file and folder has a "show" button that reveals it in Finder, Explorer or the file manager.
  It only works when the disk is plugged in and the page is served by the app (not in a standalone copy).
- `sidecar install` puts `Open Sidecar.command` (Mac, Linux) and `Open Sidecar.bat` (Windows) in the visible
  root of the disk, so nobody has to open the hidden `.sidecar` folder. Run the installer again on an
  existing disk to get them.

## 0.3.0

- New disks are noticed while the app runs: when a disk without a sidecar is plugged in, the page offers
  to build one (install and scan). "Not now" asks again next time; "Never for this disk" is remembered.
  `sidecar serve --no-watch` turns it off.

## 0.2.1

- The interface and every message are in English. The quarantine folder is `_quarantine` and the word
  that confirms permanent deletion is `DELETE`. Numbers and dates follow the browser's language.
- Fix: on Windows, `--mounts` was ignored, so disks mounted outside a drive letter were not found.

## 0.2.0

Rewritten in Go: one program per system, nothing to install first.

- A single executable per system (macOS, Linux, Windows; Intel and ARM) that contains the app, the
  scanner, the server, the installer and the MCP server. No Python, no dependencies.
- `sidecar install <disk>` copies the programs for every system it finds onto the disk, so the disk
  opens on any computer. Updating never touches the index, marks, quarantine or snapshots.
- Searching inside files is done by the server; the page receives a lean index without the extracted
  text. With two disks and 257,000 files the page's index went from 1.2 GB (38 s to load) to 100 MB (1.5 s).
- Text is no longer extracted from build output and dependency folders (`node_modules`, `.git`,
  `vcpkg_installed`, `build`, ...); only 256 KB per file at most.
- Same index layout as 0.1.0 (schema 1): indexes made by the Python prototype open unchanged.

## 0.1.0

Prototype in Python (kept in the git history, tag `v0.1.0`). Same features.

- One sidecar per disk: `<disk>/.sidecar/index.db` describes only that disk. Plugged-in disks
  are found and shown together; snapshots of other computers can be added read-only.
- Scanner (incremental), duplicate confirmation by SHA-256 (also across disks), merge of another
  index, MCP server for LLMs.
- Browser app: map of folders, search in names and contents, duplicates, folder comparison,
  marks (keep / delete / review) stored in the disk's own index.
- Quarantine: files marked for deletion are moved, not deleted, and can be restored; emptying it
  needs a typed confirmation. The server refuses to remove the last copy of a duplicated file.
- Re-check of every plugged-in disk at start, reporting what changed by hand.
- `install.py` installs or updates the sidecar on a disk without touching its data.
