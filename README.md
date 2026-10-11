# Sidecar

A catalog that lives on each of your external disks. Open it in the browser to see what is on
the disk, where the space goes, what is duplicated and what you can let go of. The same index is
a plain SQLite file, so scripts and LLMs can search it too.

![Sidecar showing a disk as a map of folders](docs/screenshot-light.png)

Plug in several disks and Sidecar shows them side by side, compares folders across disks and
finds the files that exist twice. It works on Mac and Windows, and nothing leaves your machine.

- **One catalog per disk.** `<disk>/.sidecar/index.db` describes that disk and only that disk.
  It travels with the disk and needs nobody else.
- **The disk is the truth.** The index is a snapshot. When Sidecar starts it re-checks every
  plugged-in disk and tells you what changed by hand since last time.
- **Safe clean-up.** Files you mark for deletion are *moved* to a quarantine folder on the same
  disk, not deleted. You can restore them. Space is freed only when you empty the quarantine and
  type a confirmation word. Sidecar refuses to remove the last copy of a duplicated file.
- **Search inside files.** Names, plus the text of text files, code and PDFs.
- **Made to be read by LLMs.** An MCP server and a documented schema.

## Install

You need **Python 3.9 or newer** on every computer where you create or open a catalog. There are
no other dependencies.

- Mac: `brew install python`, or the installer from [python.org](https://www.python.org/downloads/)
  (Xcode's command line tools also include a Python 3.9).
- Windows: [python.org](https://www.python.org/downloads/), tick "Add python.exe to PATH".

Then get Sidecar and install it on a disk:

```sh
git clone https://github.com/sim186/disk-sidecar
cd disk-sidecar
python3 install.py /Volumes/MyDisk        # Windows: python install.py E:\
```

The installer copies the programs into `<disk>/.sidecar/`, offers to scan the disk, and prints
the file to double click. Run it again later to update: it leaves your index, marks, quarantine
and snapshots alone. Use `--dry-run` to see what it would do, `--label "Name"` to name the disk,
`--scan` to scan without asking.

To open the catalog, double click `.sidecar/open-sidecar.command` (Mac; if macOS blocks it, right
click and choose Open) or `.sidecar/open-sidecar.bat` (Windows). Your browser opens a page served
from your own computer, `http://127.0.0.1:8765`.

Once installed, the disk is self-contained: you can plug it into any computer that has Python and
open the catalog there, without this repository.

## Using it

- **Mappa**: folders as a map sized by space, coloured by file type. Click a folder to open it,
  click a type in the legend to filter. The disks in the left column filter everything.
- **Cerca**: search names and the contents of text, code and PDF files (PDFs need `pdftotext`
  from poppler).
- **Duplicati**: groups of identical files. Run `hash_dups.py` once to turn "probably the same"
  (same name and size) into "identical" (same SHA-256).
- **Confronta**: pick two folders, even on different disks: what is only in A, only in B, in both,
  and whether one is entirely redundant.
- **Segnati**: next to every file and folder, *tieni* (keep), *elimina* (delete), *vedi* (look
  later) are notes to yourself. A mark on a folder covers what is inside. Marks are saved in the
  index of the disk the file lives on.
- **Quarantena**: see below.

The interface is in Italian. The text is inline in `sidecar/catalog.html`.

### Freeing space

1. Mark files or folders *elimina*.
2. In **Segnati**, choose "Sposta in quarantena". A progress bar shows the move. The files go to
   `<disk>/.sidecar/_quarantena/<date>/…` (a rename on the same disk, so it is instant) and leave
   the index. Nothing is deleted yet.
3. In **Quarantena** you can restore any item. When you are sure, "Elimina definitivamente…"
   deletes everything in quarantine after you type `ELIMINA`. This is the only step that cannot be
   undone, and the only one that frees space.

The server refuses, whatever the page asks: paths outside a disk, files or folders that changed
since the last scan, symlinks, anything on a read-only snapshot, and moving every remaining copy of
a duplicated file across the plugged-in disks. Every action is appended to `log.csv`.

### Several disks, and computers you cannot plug in

Start Sidecar from any disk. It looks for `.sidecar/index.db` on every mounted disk (`/Volumes`
on Mac, drive letters on Windows, `/media`, `/mnt`, `/run/media` on Linux; add more with
`--mounts DIR`) and shows them together.

To recognise copies across disks, give each disk the hashes of the files whose size also occurs
on the other (only those files are read):

```sh
python3 /Volumes/A/.sidecar/hash_dups.py --db /Volumes/A/.sidecar/index.db --also-match /Volumes/B/.sidecar/index.db
python3 /Volumes/B/.sidecar/hash_dups.py --db /Volumes/B/.sidecar/index.db --also-match /Volumes/A/.sidecar/index.db
```

A computer you cannot plug in (a laptop at home, say): scan it there into a small file,

```sh
python scan.py C:\Users\me --db laptop.db --source "Laptop"
python hash_dups.py --db laptop.db --also-match <copy of a disk's index.db>
```

and drop `laptop.db` into `<disk>/.sidecar/peers/`. It appears as a read-only snapshot with its
date. Its copies do not count as safe copies when Sidecar protects the last copy of a file.

## For LLMs and scripts

```sh
claude mcp add disk-sidecar -- python3 /Volumes/MyDisk/.sidecar/mcp_server.py
```

Tools: `disk_summary`, `search_files`, `search_content`, `get_file_info`, `list_folder`,
`find_duplicates`. Or open `index.db` read-only with any SQLite client; the schema and example
queries are in `sidecar/README.md`, which the installer puts next to the index.

## Versions and updates

Sidecar follows [semantic versioning](https://semver.org/). The version is in `sidecar/VERSION`,
the history in [CHANGELOG.md](CHANGELOG.md), and each release is a git tag (`v0.1.0`).

- Update: `git pull`, then `python3 install.py /Volumes/MyDisk` for each disk.
- The index has its own `schema_version`. A newer Sidecar opens older indexes; an older Sidecar
  refuses to touch an index written by a newer one, and tells you to update.
- To release: change `sidecar/VERSION`, add an entry to the changelog, tag and push.

## Good to know

- **exFAT** is the filesystem to use for a disk shared by Mac and Windows. It has no journal:
  unplugging during a scan or a move can corrupt the index. A backup, `index.db.bak`, is kept next
  to it; keep another copy somewhere else now and then.
- NTFS disks are read-only on a Mac without a third-party driver.
- Copying with Finder or `cp` onto exFAT/NTFS creates `._name` files; Sidecar ignores them.
- The index holds file names and extracted text. Treat it like the data it describes.
- Skipped on purpose: `$RECYCLE.BIN`, `System Volume Information`, `.Spotlight-V100`, `.Trashes`,
  `.fseventsd`, `.TemporaryItems`, `.sidecar`, `.DS_Store`, `Thumbs.db`, `desktop.ini`.

## Development

```sh
python3 -m unittest discover -s tests -v
```

The tests build fake disks in a temporary folder: scanning and incremental updates, duplicates
across disks, merging, installing and updating, and the server (disks found together, changes made
by hand, marks routed to the right disk, the quarantine and all the rules that protect your files).
Dependencies of the page are vendored in `sidecar/vendor` (sql.js, MIT; Atkinson Hyperlegible, SIL
OFL), so it works offline.

What has and has not been checked:

- The test suite passes on macOS with Python 3.9, 3.13 and 3.14. The workflow in
  `.github/workflows` runs it on Linux, macOS and Windows, but had not run when this was written.
- Nothing has been run on Windows or Linux by the author: drive letter discovery,
  `open-sidecar.bat` and the Windows paths are untested.
- The page was checked with a headless browser, not by a person clicking through every dialog.

## Licence

[MIT](LICENSE).
