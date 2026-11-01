# Ports: `sidecar scan` in C and Rust

An experiment, not part of the release. The Go program stays the product; these two ports exist to
compare Go, C and Rust on the same job, with the same output. The findings are in
[`docs/performance-go-c-rust.md`](../docs/performance-go-c-rust.md).

## What is ported

Only `sidecar scan <folder>`: the part of the program that does the heavy lifting on a disk. It is
`internal/scan` and the parts of `internal/idx` it uses:

- walk the folder depth first, entries in byte order (the order of Go's `filepath.WalkDir`), skipping
  the same folders and files, following links to files but not to folders, and leaving out program
  folders when the folder is the home folder;
- read the index already there, and skip files whose size and date did not change;
- for the others: name, lower-case extension, kind, top folder, optional SHA-256 (`--hash-min-mb`),
  and the text of text files (first 256 KiB, invalid UTF-8 replaced like Go's `strings.ToValidUTF8`;
  PDFs through `pdftotext`) into the FTS5 table;
- commit every 5000 files or 200 ms, forget files gone from the disk, clean text once from folders
  that are never searched, record the time of the scan;
- the same `[label] N files, … | new, changed, same, removed, errors | secs` line at the end.

Both ports write **the same rows, with the same ids**, as the Go program. `ports/bench` checks this on
every run: it hashes everything in the index except the random disk id and the scan time, and prints
`DIFFERENT` if an implementation disagrees.

Not ported: the server and the page, phones, thumbnails, the farewell box, merge, MCP, install.

## Layout

| | |
|---|---|
| `rust/` | Rust 2021, `rusqlite` (bundled SQLite), `sha2`, `libc`, `getrandom`. One file, `src/main.rs`. |
| `c/` | C11 + POSIX. `sidecar_scan.c`, `sha256.c/.h` (CommonCrypto on macOS, plain C elsewhere), `Makefile`. |
| `bench/` | The benchmark: builds everything, makes the fake disks, runs, compares, prints the table. |
| `bench/results/` | Measures kept from runs (JSON). |

## SQLite: the same engine everywhere (almost)

| Implementation | SQLite | How |
|---|---|---|
| `go` (the product) | 3.53.4 | `modernc.org/sqlite`: the C source machine-translated to Go, no cgo |
| `go-cgo` | 3.53.4 | `github.com/mattn/go-sqlite3`: the C source through cgo (build tags `cgosqlite sqlite_fts5`) |
| `rust` | 3.53.2 | `rusqlite` 0.40, feature `bundled`: the C source compiled by the `cc` crate at `-O3` |
| `c` | 3.53.2 | the **same file** `rusqlite` bundles, compiled with the same `-D` flags at `-O3` |

`go-cgo` is there to separate the language from the SQLite driver: it is the Go program, unchanged,
with the C SQLite instead of the translated one (`internal/idx/driver_cgo.go`; the default build is
untouched). rusqlite's SQLite is built with `SQLITE_DEFAULT_FOREIGN_KEYS=1`; both ports turn foreign
keys off on open, as SQLite and the Go program have them.

## Build and run

```sh
# Rust
cd ports/rust && cargo build --release            # target/release/sidecar-scan-rs

# C (compiles the SQLite source rusqlite downloaded: run `cargo fetch` in ports/rust first)
cd ports/c && make                                # build/sidecar-scan-c
make SHA=portable                                 # plain C SHA-256 instead of CommonCrypto
make SQLITE=system                                # link the system libsqlite3 instead

# the same flags as `sidecar scan`
sidecar-scan-rs <folder> [--db index.db] [--source name] [--hash-min-mb N] [--no-text] [--text]
sidecar-scan-c  <folder> [--db index.db] [--source name] [--hash-min-mb N] [--no-text] [--text]

# the benchmark (from the repository root)
go run ./ports/bench                              # 200,000 files + 2 GB to hash, 5 runs each
go run ./ports/bench -files 50000 -runs 3         # quicker
go run ./ports/bench -build-times -json out.json  # also time builds; keep the measures
```

The benchmark needs Go, a Rust toolchain, a C compiler and `make`; it puts its fake disks in the
temporary folder (`-work` to choose) and reuses them between runs.

## Known differences

- **Extensions with non-ASCII letters.** Go lower-cases them with Unicode rules. Rust does the same
  except for the few letters whose lower case is two characters (it keeps those as they are, Go maps
  one rune to one rune). C lower-cases ASCII only: Unicode case tables are not in the C library.
  Kinds are only ever ASCII extensions, so only the `ext` column of such files can differ.
- **Error messages** (on stderr) are worded by each language's library.
- `SIDECAR_SCAN_FOREGROUND=1` keeps the scan out of the background band (all three implementations).
  Only the benchmark sets it; see the performance document for why.
