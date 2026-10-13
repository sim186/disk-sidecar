# Changelog

All notable changes to Disk Sidecar. Versions follow [Semantic Versioning](https://semver.org/);
the layout of the index has its own number (`schema_version` in the index's `meta` table),
which only changes when older programs could no longer read or write it safely.

## Unreleased

- This computer no longer slows the computer down. Installing on the system disk scanned the whole home folder and
  read the text of every text, code and PDF file, then did it again each time the app started.
  - The home folder is catalogued by name only (`sidecar scan ~ --text` reads text too), and hidden folders,
    `node_modules`, `venv`, `__pycache__`, `site-packages`, `Pods` and `vcpkg_installed` are left out at any depth.
  - The check at start leaves out this computer; **Check now** in the app still includes it.
  - Files only in the cloud (OneDrive Files On-Demand, iCloud Drive with Optimise Mac Storage) are listed but never
    read, so scans and photo previews no longer download them.
  - Scans run at background CPU and disk priority on macOS, Windows and Linux, on every disk.
  - On Windows the `.sidecar` folder is also left out of Windows Search.

## 0.8.0

- Map: switch between **Folders** and **All files** (a flat list of the largest files, sortable by name, size or
  date), and hide the map with **Hide map** (remembered). Rows can be ticked, and a bar answers Keep, Not sure or
  Let go for all ticked rows at once, in the Map, Search and Duplicates. Photos on a disk also show previews on the Map.
- Map: a **Photos** view shows the photos and videos of the current folder as a grid of larger previews (256 px,
  cached apart from the small ones), 120 at a time, with a tick box on every tile. Right-click a tile (or press the menu
  key) to answer Keep, Not sure or Let go, copy it, or show it in the file manager; with several tiles ticked the menu
  answers for all of them. A small badge on the tile shows its answer. Videos and files without a preview show their type. Phones show types only: they are never read for previews.
- Tables: the answer is a fixed three-way switch (Keep, Not sure, Let go) with the chosen one filled and a coloured
  edge on the row; paths show the folder quietly above the file name; Share has a bar; the side bar is tighter.
- Fixed: previews and the Copy to / Show links only appeared after the first click, not on the first draw.
- Previews. Photos (jpg, png, gif, webp, bmp, tiff) on a disk that is plugged in show a small picture next to their
  name in Search, Duplicates and Tidying piles. They are drawn only when scrolled into view, a JPEG's own embedded
  thumbnail is used when it has one, at most three are made at a time, and results are cached in the user's cache
  folder. Phones are never read for previews. **Show previews of photos** in the side bar turns them off.

## 0.7.0

- The word that confirms releasing the farewell box for good is now `let it go` (was `DELETE`), in the app and in
  the `/api/empty` call. Case and surrounding spaces do not matter.
- Phones. **Add a phone…** in the side bar (or `sidecar phone`, `sidecar phone scan <serial>`) catalogues a
  phone so it appears next to your disks: map, duplicates, compare, search and the tidying piles' view all work on it.
  A phone cannot carry a sidecar, so its catalog is kept on this computer in `~/.sidecar/phones`, and the phone is
  only changed by the two actions below, never by a scan.
  - Android: read through `adb` (USB debugging on). Lists the shared storage (`/sdcard`) without copying anything.
  - iPhone: only the camera roll (DCIM), through `ifuse`/libimobiledevice. Apple keeps app data out of reach.
- Android phones can be emptied safely. **copy to…** pulls files to a disk: each is hashed, compared with the
  hash the phone itself reports, and only then given its name; nothing is overwritten. **let go from phone…** releases
  from the phone only files that have an identical copy on a connected disk, where both the phone and the disk
  file are read again at that moment (a damaged or changed copy proves nothing). It shows the result first
  and asks you to type `let it go`, because the phone has no farewell box. iPhones stay read only.

## 0.6.0

- Copy to another disk or folder (**copy to…** next to files and folders, and for the whole *keep* pile). Files are
  verified after copying, never overwritten, and indexed at once. Moving is copy, then let the original go.
- "This computer": Sidecar can be installed on the system disk (in `~/.sidecar`, cataloguing the home folder). There
  it refuses to move, restore or copy anything outside the user's own folders.
- A gentler vocabulary, after Marie Kondo. In the app, **Marked** is now *Tidying piles* (*keep*, *spark joy?*,
  *let go*) and **Quarantine** is the *Farewell box*, where things are thanked before they are released for
  good. Only the words changed: the index, the API, the MCP server and the `DELETE` confirmation are the same.
- Fixed: with the install dialog open, plugging in a disk also raised the corner prompt, and accepting that one
  left the dialog stuck on top. Now the dialog adds the new disk to its list, and accepting the corner prompt
  closes the dialog.

## 0.5.0

- Double clicking the program (no command) opens the app. If no disk has a sidecar yet it asks where to
  install it, with a list of the connected disks; "Install on a disk…" in the side bar does the same later.
  The terminal is no longer needed to install.
- The app tells you when a newer release exists (a line under the name, with a link and "skip"). It asks
  GitHub at most once a day and sends only the request itself: no disk, file or user information. Nothing is
  downloaded or installed by itself. Turn it off with `sidecar serve --no-update-check` or the environment
  variable `SIDECAR_NO_UPDATE_CHECK=1`.
- The page has a favicon.
- Fix: a disk reached through a symbolic link (for example `/tmp` on a Mac) was offered as new even when it
  already had a sidecar.

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
