# Go, C and Rust: the same scanner, three times

*Measured 1 November 2026 on branch `ports-c-rust`. Raw data: [`ports/bench/results/m4-200k.json`](../ports/bench/results/m4-200k.json).
How the ports are built and what they cover: [`ports/README.md`](../ports/README.md).*

## In short

- **C and Rust ran at the same speed,** about **1.5× faster than Go** (0.64–0.73× of Go's time) wherever
  the work is "many small files into SQLite". The gap between C and Rust is within the noise.
- **Hashing large files is a tie.** All four implementations (the fourth is Go with the C SQLite, below)
  hash at roughly 2.3–2.6 GB/s. Every one of them most likely uses the CPU's SHA-256 instructions,
  so the language barely matters.
- **Go uses 2.5–4× more memory** (100–136 MB against 25–48 MB): a garbage-collected heap, plus the
  memory emulation the translated SQLite needs.
- **Swapping Go's SQLite for the real C one did not make Go faster** (between −7% and +8%). The C
  engine is faster, but calling it through cgo lost that gain again (see [Where the time goes](#where-the-time-goes)).
- **The biggest factor is not the language.** Running the scan in the background band, as the shipped
  program does, makes every implementation **5× slower** (Go: 10 s → 51 s). On the slow USB disks
  Sidecar is made for, the disk dominates even more.
- **The price of the speed:** C needed about 45% more code and everything written by hand. Rust
  needed about 10% more code, 45 crates and slower builds. **For Disk Sidecar, Go stays the right
  choice**; the reasons are at the end.

## What was compared

The job is `sidecar scan <folder>`, the heavy part of the program. It walks a disk, reads each
file's size and date, optionally computes SHA-256, reads the text of text files, and writes everything
into the SQLite index. Rescans skip files that did not change and remove files that are gone. It is a
realistic mix of system calls, hashing, UTF-8 handling, hash maps and SQLite inserts.

| Implementation | Language code | SQLite |
|---|---|---|
| **go** (the product, unchanged) | `internal/scan`, `internal/idx` | `modernc.org/sqlite` 3.53.4: the C source machine-translated to Go |
| **go-cgo** | same Go code | `mattn/go-sqlite3` 3.53.4: the real C SQLite through cgo |
| **c** | `ports/c`, C11 + POSIX | 3.53.2, the same source file rusqlite bundles, same `-D` flags, `-O3` |
| **rust** | `ports/rust`, Rust 2021 | `rusqlite` 0.40 `bundled`: 3.53.2 compiled by the `cc` crate, `-O3` |

Rules that keep the comparison fair:

- **Same algorithm.** The two ports translate the Go code: single-threaded, the same walk order, the
  same SQL statements, prepared once, and the same commit cadence (every 5000 files or 200 ms). They
  do the work the Go code does, nothing cleverer.
- **Same output.** After every scenario the benchmark hashes the index each implementation wrote
  (every row of every table, except the random disk id and the scan time). **All four wrote identical
  indexes in all six scenarios**, rows and ids included. The test tree covers invalid UTF-8 in text
  files, accented and CJK names, upper-case extensions, symlinks to files and to folders, `._` files,
  `.DS_Store` and folders whose text is never indexed.
- **Same SQLite.** The engine is the same version (3.53.x) everywhere. C and Rust compile literally the
  same `sqlite3.c`. rusqlite turns foreign keys on by default; both ports turn them off, as SQLite
  itself and the Go program have them.
- **Optimised builds.** Go `-trimpath -ldflags "-s -w"`. Rust `--release` with LTO and one codegen
  unit. C `-O3`.
- **Same scheduling.** The shipped program puts the scan thread in the background band. On Apple
  silicon that moves it to the efficiency cores and throttles its I/O. All three implementations do
  this. The benchmark turns it off (`SIDECAR_SCAN_FOREGROUND=1`) to time the code rather than the
  scheduler, except in one scenario that measures it.

## Method

- **Machine:** Apple M4 (10 cores), 24 GB RAM, internal SSD, macOS 26. Go 1.26.0, Rust 1.97.1,
  Apple clang 21.
- **Data:**
  - *Tree:* 200,000 files in 5,077 folders. About 30% are text files of 0.2–12 KB; the rest are
    sparse files with realistic sizes up to 40 MB.
  - *Big:* 32 files of 64 MB of random bytes, 2 GB in total.
- **Runs:** each scenario runs once per implementation without being timed (this warms the page
  cache and produces the index to compare). Then come 5 timed runs, rotating through the
  implementations so that drift in the machine affects all of them alike. The tables show the
  median; spread between runs was 1–5% (up to 26% in the background scenario).
- **Measures:** wall time; CPU time (user + system, from `wait4`); peak resident memory.
- **Not measured:** a cold page cache or a USB hard disk. With a warm cache this times the programs,
  not the disk; on a real USB disk all four would be closer together.

## Results

Time relative to Go: lower is better.

| Scenario | go | go-cgo | c | rust |
|---|---:|---:|---:|---:|
| **scan_names**: new index, names only | 5.28 s | 1.08× | **0.64×** | **0.64×** |
| **scan_text**: new index, names + text | 9.95 s | 0.93× | **0.64×** | 0.66× |
| **rescan_same**: nothing changed (walk, stat, compare) | 0.91 s | 0.95× | **0.65×** | 0.73× |
| **rescan_stale**: 10% changed, 5% new, 5% gone | 2.97 s | 0.96× | **0.69×** | 0.71× |
| **hash**: SHA-256 of 2 GB | 0.81 s | 1.00× | 0.97× | 1.10× |
| **scan_text_bg**: scan_text in the background band, as shipped | 51.1 s | 1.00× | **0.65×** | 0.67× |

CPU time (user + system):

| Scenario | go | go-cgo | c | rust |
|---|---:|---:|---:|---:|
| scan_names | 6.27 s | 7.05 s | 3.20 s | 3.19 s |
| scan_text | 10.83 s | 10.70 s | 6.11 s | 6.28 s |
| rescan_stale | 2.96 s | 2.97 s | 1.75 s | 1.83 s |

Go's CPU time is *higher than its wall time*: the scan itself runs on one thread, but the runtime's
garbage collector and idle scheduler threads run alongside it on other cores. C and Rust spend less
CPU than wall time; the rest is waiting for the disk (`fsync`).

Peak memory:

| Scenario | go | go-cgo | c | rust |
|---|---:|---:|---:|---:|
| scan_names | 106 MB | 99 MB | 48 MB | **25 MB** |
| rescan_stale | 136 MB | 129 MB | 47 MB | **44 MB** |
| hash | 33 MB | 28 MB | **7 MB** | **7 MB** |

C uses twice Rust's memory on a new scan. The cause is my C hash table, not the language. Every slot
is 40 bytes and every path is its own `malloc` block. When the table doubles, the old and new arrays
exist at the same time. Rust's `HashSet` is more compact. Writing an equally good table in C takes
more work; that is part of what C costs.

### Building and the code

| | go | go-cgo | c | rust |
|---|---:|---:|---:|---:|
| Program size | 12.6 MB¹ | 10.4 MB¹ | 1.7 MB | 2.2 MB |
| Clean build (empty caches, dependencies included) | 6–7 s | 15 s | 11 s² | 16 s |
| Incremental build (one source file changed) | 0.2 s | 0.2 s | 0.2 s | 2.8–5.9 s³ |
| Lines of scan code (no blanks or comments) | ≈ 580⁴ | same | 839 | 637 |
| Third-party code | modernc (≈ 10 modules) | + mattn | none (SQLite source) | 45 crates in `Cargo.lock` |
| `unsafe` / raw memory | none | cgo | all of it | 3 small blocks (`setpriority`, `gethostname`) |

1. The Go binary is the **whole** program: server, MCP, phones, thumbnails, and the web page with its
   fonts and its 1 MB SQLite-in-WebAssembly. It is not comparable with the scanner-only ports.
2. 10 of the 11 seconds compile `sqlite3.c`.
3. Release profile with LTO and one codegen unit. These settings make the program fast and every
   rebuild slow; a debug build is much quicker.
4. The parts of `scan.go`, `idx.go` and `write.go` that the scan uses.

## Where the time goes

I sampled complete `scan_names` runs (macOS `sample`, 1 ms, idle threads left out). Figures are
milliseconds of busy time:

| | go (modernc) | go-cgo | c |
|---|---:|---:|---:|
| Kernel: SQLite writing its file (`pwrite`, `fsync`, …) | 1,830 | 1,910 | 1,620 |
| Kernel: walking (`getdirentries`, `lstat`) | 540 | 420 | 390 |
| SQLite engine | 1,220 + 300 libc emulation | 1,340 | **360** |
| Go runtime (scheduler, garbage collector, allocation) | 850 | 770 | — |
| Other (program, libc, allocator) | 200 | 1,230 | 640 |

What this shows:

1. **The kernel work is the same everywhere:** 2–2.3 s. Same SQL, same pages written, same files
   read. No language changes this. It is the floor.
2. **Translated SQLite costs about 4× native SQLite** (≈ 1,500 ms against 360 ms). modernc turns C
   into Go that emulates C's memory: every `malloc` and every stack variable whose address is taken
   goes through `modernc.org/libc`. This one thing explains most of the gap between Go and C or Rust.
3. **The Go runtime adds about 0.8 s.** Partly real work (garbage collection, allocation), partly
   threads looking for work (`runtime.stealWork`), which costs CPU but not wall time.
4. **Why go-cgo is not faster.** Its SQLite engine is real C, yet SQLite's page cache allocates
   (`pcache1FetchStage2`, 850 ms, plus 280 ms of `madvise`). With the very same SQLite source, the C
   port shows almost none of this. So the cost comes from the C memory allocator when it runs under
   cgo. My best explanation, *not proven*: Go moves goroutines between OS threads, so the C
   allocator's per-thread caches miss. The cgo calls themselves are cheap (56 ms). **Lesson:** "call
   the C library" is not automatically faster; measure it.

## Surprises

- **Background priority costs 5×, language 1.5×.** The shipped scan runs politely in the background
  band (on purpose: a scan should not slow the computer down). On an M4 that means efficiency cores.
  If scan speed ever matters, let the user ask for a foreground scan; that is a 5× gain without
  rewriting anything.
- **Rust's speed comes from the same place as C's:** native SQLite, no garbage collector, compact
  data. Rust was within 2–4% of C in most scenarios. C won by 11% on `rescan_same` and Rust lost 13%
  on hashing (the `sha2` crate against Apple's CommonCrypto); neither is about the language.
- **Library defaults matter.** rusqlite silently compiles SQLite with foreign keys on, and Go's
  `filepath.WalkDir` sorts every folder. Matching the three programs meant finding such defaults one
  by one. Without the index check in the benchmark, a port can look faster only because it does
  less.

## What writing each one was like

**C.** Everything the other two get from the standard library had to be written:
- a hash table;
- a growable buffer;
- UTF-8 validation with Go's replacement rules;
- SHA-256 (CommonCrypto on macOS, plus a portable fallback);
- spawning `pdftotext` with a timeout;
- recursion over directory entries.

It also only lower-cases ASCII extensions, because Unicode case tables are not in the C library.
While writing it I made three mistakes that the compiler did not catch:
- a typo in the list of skipped folders (`TemporaryItems` without the dot);
- `memrchr`, which does not exist on macOS (this one at least failed to build);
- missing `#include`s.

The first would have indexed folders it should skip, silently. Memory is managed by hand everywhere;
every early `continue` in the walk has to free the name it owns.

**Rust.** The code is close to the Go code: `HashMap`, `utf8_chunks`, `fs::read_dir`, and
`rusqlite`'s `prepare_cached` instead of a manual statement cache. Keeping paths as raw bytes
(a Unix file name need not be UTF-8) took a small custom `ToSql`. The compiler caught borrow mistakes
before they ran. The only `unsafe` blocks are three system calls. The costs:
- 45 crates for a 640-line program (mostly build tools: `cc`, `pkg-config`, `vcpkg`, …);
- slow optimised builds;
- the borrow checker shaping the code: the scanner's state lives in one struct, because closures
  could not each hold a mutable borrow.

**Go.** The existing code is the shortest of the three. It cross-compiles to six systems without a
C compiler (the reason modernc was chosen), and builds in a fraction of a second.

## So, why is everything being rewritten in Rust?

This experiment matches the usual story, with nuance:

- **Speed and memory like C, with memory safety.** That combination is the real argument. Rust
  matched C within a few percent, and it used the least memory, without anyone having to manage
  memory by hand. The C version is just as fast, but every `malloc`, `free` and buffer length is a
  chance for a bug that crashes the program or corrupts the index. For code that parses untrusted
  input (file names, file contents, network data), this is why projects such as browsers, operating
  system kernels and command-line tools move to Rust.
- **No garbage collector.** Memory use is predictable and there are no runtime threads; that matters
  for libraries, embedded systems and latency-sensitive servers. Here it meant 25 MB instead of
  106 MB.
- **Cargo.** Adding SQLite (bundled and compiled) and SHA-256 took two lines. In C, using the same
  SQLite meant hunting for its source and copying 20 compiler flags by hand.

What the trend tends to leave out:

- **Go was 1.5× slower, not 10×,** on a CPU-heavy, warm-cache test. Most of the difference is one
  dependency (translated SQLite), not Go itself.
- **The disk and the scheduler matter more:** 2 s of every run is kernel I/O in all four languages,
  and background priority costs 5×.
- **Rust has costs:** compile times, a dependency tree to audit, and a steeper learning curve.

### For Disk Sidecar

Stay with Go. The user waits on USB disks and on a scan that is deliberately run in the background,
not on CPU. Go's cross-compilation without a C toolchain is worth more to this project than 1.5×
CPU. If scanning speed ever becomes a complaint, these would help, roughly in order of payoff:
1. Offer a foreground ("scan now") mode: about 5×.
2. Batch several files per SQLite statement, or skip the `RETURNING` round trip for files without
   text: less SQLite engine work, which is Go's weak spot here.
3. Only then consider a native SQLite. Measure it under cgo first, because here it brought nothing.

## Limits of this test

- One machine (Apple M4, macOS), warm page cache, internal SSD. Linux, Windows and USB hard disks
  were not measured.
- All ports are single-threaded, like the Go scanner. A parallel walker would change the picture,
  in every language.
- The profile is a wall-clock sampler with ±10% noise between runs, good for orders of magnitude.
  The go-cgo allocator explanation is a hypothesis.
- PDF text extraction (`pdftotext`, an external program) is implemented in every port but not
  benchmarked.

## Reproduce

```sh
git checkout ports-c-rust
go run ./ports/bench                                   # about 30 minutes; the background scenario is most of it
go run ./ports/bench -only scan_names,hash -runs 3     # a few minutes
go run ./ports/bench -build-times -only none           # build times only
```

You need Go, a Rust toolchain (`rustup`), a C compiler and `make`. The fake disks (about 2.5 GB)
go to `$TMPDIR/sidecar-ports-bench`; use `-work` to put them elsewhere.
