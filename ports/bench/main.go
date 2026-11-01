// Command bench compares the Go, C and Rust implementations of `sidecar scan` (see ports/README.md).
//
// It builds every implementation, makes two fake disks (a tree of many small files, and a few big
// files for hashing), runs each scenario on each implementation in turn, checks that they all wrote
// the same index, and prints a table (and, with -json, writes every measure to a file).
//
//	go run ./ports/bench                          200,000 files, 5 runs per measure
//	go run ./ports/bench -files 50000 -runs 3     quicker
//	go run ./ports/bench -build-times             also time clean and incremental builds
//
// Timings are with a warm page cache (one unmeasured run first): this times the programs, not the
// disk. SIDECAR_SCAN_FOREGROUND=1 is set unless a scenario says otherwise: shipped builds put the scan
// thread in the background band, which on Apple silicon means the efficiency cores.
package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/sim186/disk-sidecar/internal/idx"
)

var (
	flagFiles   = flag.Int("files", 200_000, "files in the tree")
	flagBigMB   = flag.Int("big-mb", 2048, "megabytes of big files to hash")
	flagRuns    = flag.Int("runs", 5, "measured runs per scenario and implementation")
	flagWork    = flag.String("work", filepath.Join(os.TempDir(), "sidecar-ports-bench"), "folder for the fake disks and indexes")
	flagOnly    = flag.String("only", "", "comma-separated scenarios to run (default all)")
	flagImpls   = flag.String("impls", "go,go-cgo,c,rust", "comma-separated implementations")
	flagJSON    = flag.String("json", "", "write the measures to this file")
	flagBuildT  = flag.Bool("build-times", false, "also time clean and incremental builds")
	flagNoBuild = flag.Bool("no-build", false, "use the binaries already in <work>/bin")
)

type impl struct {
	name, bin string
}

type scenario struct {
	name, desc string
	tree       string   // "tree" or "big"
	start      string   // "" = new index, "template" = complete index, "stale" = index out of date
	args       []string // extra flags
	background bool     // leave the background band on, as shipped
}

var scenarios = []scenario{
	{"scan_names", "new index, names only (--no-text)", "tree", "", []string{"--no-text"}, false},
	{"scan_text", "new index, names + text of text files", "tree", "", nil, false},
	{"rescan_same", "index up to date: walk + stat + compare, nothing written", "tree", "template", nil, false},
	{"rescan_stale", "10% changed, 5% new, 5% gone since the index was written", "tree", "stale", nil, false},
	{"hash", "new index, SHA-256 of every file (--hash-min-mb 0)", "big", "", []string{"--no-text", "--hash-min-mb", "0"}, false},
	{"scan_text_bg", "scan_text in the background band, as shipped", "tree", "", nil, true},
}

type measure struct {
	Scenario, Impl string
	WallMS         []float64
	CPUMS          []float64 // user + system
	MaxRSSMB       []float64
	Digest         string // of the index written, to compare implementations
}

func main() {
	flag.Parse()
	repo := findRepo()
	work, _ := filepath.Abs(*flagWork)
	must(os.MkdirAll(work, 0o755))
	bin := filepath.Join(work, "bin")
	must(os.MkdirAll(bin, 0o755))

	all := map[string]impl{
		"go":     {"go", filepath.Join(bin, "sidecar-go")},
		"go-cgo": {"go-cgo", filepath.Join(bin, "sidecar-go-cgo")},
		"c":      {"c", filepath.Join(bin, "sidecar-scan-c")},
		"rust":   {"rust", filepath.Join(bin, "sidecar-scan-rs")},
	}
	var impls []impl
	for _, n := range strings.Split(*flagImpls, ",") {
		im, ok := all[strings.TrimSpace(n)]
		if !ok {
			fail("unknown implementation %q", n)
		}
		impls = append(impls, im)
	}

	if !*flagNoBuild {
		buildAll(repo, bin, impls)
	}
	if *flagBuildT {
		buildTimes(repo, work)
	}
	fmt.Println("\nbinaries:")
	for _, im := range impls {
		st, err := os.Stat(im.bin)
		must(err)
		fmt.Printf("  %-7s %6.2f MB  %s\n", im.name, float64(st.Size())/1e6, im.bin)
	}

	tree := filepath.Join(work, "tree")
	big := filepath.Join(work, "big")
	genTree(tree, *flagFiles)
	genBig(big, *flagBigMB)

	// the complete index the rescans start from, and a stale copy of it
	tmpl := filepath.Join(work, "template.db")
	stale := filepath.Join(work, "stale.db")
	os.Remove(tmpl)
	runOnce(impls[0], tree, tmpl, nil, false)
	copyFile(tmpl, stale)
	makeStale(stale)

	var results []measure
	for _, sc := range scenarios {
		if *flagOnly != "" && !slices.Contains(strings.Split(*flagOnly, ","), sc.name) {
			continue
		}
		fmt.Printf("\n== %s: %s\n", sc.name, sc.desc)
		root := tree
		if sc.tree == "big" {
			root = big
		}
		ms := make([]measure, len(impls))
		for i, im := range impls {
			ms[i] = measure{Scenario: sc.name, Impl: im.name}
		}
		db := filepath.Join(work, "run.db")
		prepare := func() {
			for _, s := range []string{"", "-journal", "-wal", "-shm"} {
				os.Remove(db + s)
			}
			switch sc.start {
			case "template":
				copyFile(tmpl, db)
			case "stale":
				copyFile(stale, db)
			}
		}
		// one unmeasured run each: warms the page cache, and gives the index to compare
		for i, im := range impls {
			prepare()
			runOnce(im, root, db, sc.args, sc.background)
			ms[i].Digest = digest(db)
		}
		for r := 0; r < *flagRuns; r++ {
			for i, im := range impls { // in turn, so drift in the machine hits everyone alike
				prepare()
				wall, cpu, rss := runOnce(im, root, db, sc.args, sc.background)
				ms[i].WallMS = append(ms[i].WallMS, wall)
				ms[i].CPUMS = append(ms[i].CPUMS, cpu)
				ms[i].MaxRSSMB = append(ms[i].MaxRSSMB, rss)
			}
		}
		base := median(ms[0].WallMS)
		fmt.Printf("  %-7s %10s %10s %10s %10s %8s  %s\n", "impl", "wall p50", "wall min", "cpu p50", "rss p50", "vs "+impls[0].name, "index")
		for _, m := range ms {
			same := "same"
			if m.Digest != ms[0].Digest {
				same = "DIFFERENT"
			}
			fmt.Printf("  %-7s %8.0fms %8.0fms %8.0fms %8.1fMB %7.2fx  %s\n", m.Impl, median(m.WallMS), slices.Min(m.WallMS),
				median(m.CPUMS), median(m.MaxRSSMB), median(m.WallMS)/base, same)
		}
		results = append(results, ms...)
	}
	for _, m := range results {
		if m.Digest != results[0].Digest && m.Scenario == results[0].Scenario {
			fmt.Printf("\nWARNING: %s/%s wrote a different index than %s\n", m.Scenario, m.Impl, results[0].Impl)
		}
	}
	if *flagJSON != "" {
		b, _ := json.MarshalIndent(map[string]any{
			"when": time.Now().Format(time.RFC3339), "goos": runtime.GOOS, "goarch": runtime.GOARCH, "cpus": runtime.NumCPU(),
			"files": *flagFiles, "big_mb": *flagBigMB, "runs": *flagRuns, "measures": results,
		}, "", "  ")
		must(os.WriteFile(*flagJSON, b, 0o644))
	}
}

// ------------------------------------------------------------------ building

func buildAll(repo, bin string, impls []impl) {
	fmt.Println("building:")
	for _, im := range impls {
		t0 := time.Now()
		switch im.name {
		case "go":
			sh(repo, []string{"CGO_ENABLED=0"}, "go", "build", "-trimpath", "-ldflags", "-s -w", "-o", im.bin, "./cmd/sidecar")
		case "go-cgo":
			sh(repo, []string{"CGO_ENABLED=1"}, "go", "build", "-trimpath", "-ldflags", "-s -w", "-tags", "cgosqlite sqlite_fts5", "-o", im.bin, "./cmd/sidecar")
		case "rust":
			sh(filepath.Join(repo, "ports/rust"), nil, "cargo", "build", "--release", "--quiet")
			copyFile(filepath.Join(repo, "ports/rust/target/release/sidecar-scan-rs"), im.bin)
		case "c":
			sh(filepath.Join(repo, "ports/rust"), nil, "cargo", "fetch", "--quiet") // the SQLite source the C port compiles
			sh(filepath.Join(repo, "ports/c"), nil, "make", "--quiet")
			copyFile(filepath.Join(repo, "ports/c/build/sidecar-scan-c"), im.bin)
		}
		os.Chmod(im.bin, 0o755)
		fmt.Printf("  %-7s %.1fs\n", im.name, time.Since(t0).Seconds())
	}
}

// buildTimes times a build from nothing (empty caches, dependencies included) and an incremental
// build after touching the main source file, for each language.
func buildTimes(repo, work string) {
	fmt.Println("\nbuild times (clean = empty caches, dependencies included; incremental = main source touched):")
	tmp := filepath.Join(work, "buildcache")
	timeit := func(f func()) float64 { t0 := time.Now(); f(); return time.Since(t0).Seconds() }
	// a real change (Go's cache keys on contents, so touching a file is not enough), undone after
	var restore []func()
	defer func() {
		for i := len(restore) - 1; i >= 0; i-- { // newest first: a file touched twice gets its first contents back
			restore[i]()
		}
	}()
	touch := func(p string) {
		b, err := os.ReadFile(p)
		must(err)
		must(os.WriteFile(p, append(slices.Clone(b), "\n// bench: incremental build\n"...), 0o644))
		future := time.Now().Add(2 * time.Second) // make on macOS compares times to the second
		os.Chtimes(p, future, future)
		restore = append(restore, func() { os.WriteFile(p, b, 0o644) })
	}
	out := filepath.Join(tmp, "out")

	for _, v := range []struct {
		name string
		env  []string
		tags string
	}{{"go", []string{"CGO_ENABLED=0"}, ""}, {"go-cgo", []string{"CGO_ENABLED=1"}, "cgosqlite sqlite_fts5"}} {
		os.RemoveAll(tmp)
		env := append(v.env, "GOCACHE="+filepath.Join(tmp, "gocache"))
		args := []string{"build", "-trimpath", "-ldflags", "-s -w", "-o", out}
		if v.tags != "" {
			args = append(args, "-tags", v.tags)
		}
		args = append(args, "./cmd/sidecar")
		clean := timeit(func() { sh(repo, env, "go", args...) })
		touch(filepath.Join(repo, "internal/scan/scan.go"))
		incr := timeit(func() { sh(repo, env, "go", args...) })
		fmt.Printf("  %-7s clean %6.1fs   incremental %5.1fs\n", v.name, clean, incr)
	}
	os.RemoveAll(tmp)
	renv := []string{"CARGO_TARGET_DIR=" + filepath.Join(tmp, "target")}
	clean := timeit(func() { sh(filepath.Join(repo, "ports/rust"), renv, "cargo", "build", "--release", "--quiet") })
	touch(filepath.Join(repo, "ports/rust/src/main.rs"))
	incr := timeit(func() { sh(filepath.Join(repo, "ports/rust"), renv, "cargo", "build", "--release", "--quiet") })
	fmt.Printf("  %-7s clean %6.1fs   incremental %5.1fs\n", "rust", clean, incr)
	sh(filepath.Join(repo, "ports/c"), nil, "make", "--quiet", "clean")
	clean = timeit(func() { sh(filepath.Join(repo, "ports/c"), nil, "make", "--quiet") })
	touch(filepath.Join(repo, "ports/c/sidecar_scan.c"))
	incr = timeit(func() { sh(filepath.Join(repo, "ports/c"), nil, "make", "--quiet") })
	fmt.Printf("  %-7s clean %6.1fs   incremental %5.1fs\n", "c", clean, incr)
	os.RemoveAll(tmp)
	for i := len(restore) - 1; i >= 0; i-- {
		restore[i]()
	}
	restore = nil
	if *flagOnly == "none" {
		os.Exit(0)
	}
}

// ------------------------------------------------------------------ running

// runOnce runs one scan and returns wall time, CPU time (user+system) in ms, and peak memory in MB.
func runOnce(im impl, root, db string, extra []string, background bool) (wall, cpu, rssMB float64) {
	var args []string
	if strings.HasPrefix(im.name, "go") {
		args = append(args, "scan")
	}
	args = append(args, root, "--db", db, "--source", "bench")
	args = append(args, extra...)
	cmd := exec.Command(im.bin, args...)
	cmd.Env = os.Environ()
	if !background {
		cmd.Env = append(cmd.Env, "SIDECAR_SCAN_FOREGROUND=1")
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	t0 := time.Now()
	out, err := cmd.Output()
	wall = float64(time.Since(t0).Microseconds()) / 1000
	if err != nil {
		fail("%s %v: %v\n%s%s", im.name, args, err, out, stderr.String())
	}
	ru := cmd.ProcessState.SysUsage().(*syscall.Rusage)
	cpu = float64(ru.Utime.Nano()+ru.Stime.Nano()) / 1e6
	rss := float64(ru.Maxrss)
	if runtime.GOOS != "darwin" { // Linux reports kilobytes, macOS bytes
		rss *= 1024
	}
	return wall, cpu, rss / (1 << 20)
}

// digest is a hash of everything an index holds that does not change from run to run (not the
// random disk id, not the time of the scan): two implementations that agree on it wrote the same rows.
func digest(db string) string {
	d, err := sql.Open(idx.Driver, idx.FileURI(db, "mode=ro"))
	must(err)
	defer d.Close()
	h := sha256.New()
	for _, q := range []string{
		`SELECT key, value FROM meta WHERE key <> 'disk_id' ORDER BY key`,
		`SELECT id, label, root, host FROM sources ORDER BY id`,
		`SELECT id, source_id, path, name, ext, size, mtime, kind, top, sha256, seen FROM files ORDER BY id`,
		`SELECT rowid, path, body FROM file_text ORDER BY rowid`,
		`SELECT COUNT(*) FROM marks`,
	} {
		rows, err := d.Query(q)
		must(err)
		cols, _ := rows.Columns()
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		for rows.Next() {
			must(rows.Scan(ptrs...))
			for _, v := range vals {
				fmt.Fprintf(h, "%T:%v\x00", v, v)
			}
			h.Write([]byte{'\n'})
		}
		must(rows.Err())
		rows.Close()
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// makeStale changes an index so a rescan finds 10% of the files changed, 5% new and 5% gone.
func makeStale(db string) {
	d, err := sql.Open(idx.Driver, idx.FileURI(db, idx.BusyParam))
	must(err)
	defer d.Close()
	for _, q := range []string{
		`UPDATE files SET mtime = mtime - 1 WHERE id % 10 = 3`,
		`DELETE FROM file_text WHERE rowid IN (SELECT id FROM files WHERE id % 20 = 7)`,
		`DELETE FROM files WHERE id % 20 = 7`,
		`INSERT INTO files(source_id, path, name, ext, size, mtime, kind, top, seen)
		 SELECT source_id, 'gone/' || path, name, ext, size, mtime, kind, 'gone', 1 FROM files WHERE id % 20 = 11`,
	} {
		_, err := d.Exec(q)
		must(err)
	}
}

// ------------------------------------------------------------------ the fake disks

var (
	tops  = []string{"Photos", "Videos", "Music", "Projects", "Documents", "Backup 2019", "Camera Uploads", "Work", "Archive", "Scans", "Downloads", "Lightroom", "Sync", "Old Mac", "Phone dump", "Clients", "Taxes", "Books", "Games", "Misc"}
	words = strings.Fields("the a disk file folder photo copy keep let go sidecar index scan text search catalog backup old new holiday " +
		"summer project report draft final invoice note list music video camera café naïve Größe 日本 데이터 ✓ data byte page")
	// extension, weight, text
	exts = []struct {
		ext  string
		w    int
		text bool
	}{
		{"jpg", 26, false}, {"JPG", 6, false}, {"png", 5, false}, {"heic", 4, false}, {"cr2", 4, false}, {"mp4", 3, false},
		{"MOV", 2, false}, {"mp3", 5, false}, {"flac", 1, false}, {"docx", 3, false}, {"xlsx", 1, false}, {"zip", 2, false},
		{"dmg", 1, false}, {"lrcat", 1, false}, {"dat", 4, false}, {"", 2, false}, {"tar.gz", 1, false},
		{"txt", 6, true}, {"md", 4, true}, {"go", 3, true}, {"py", 3, true}, {"js", 3, true}, {"json", 3, true}, {"csv", 2, true},
		{"html", 2, true}, {"c", 1, true}, {"rs", 1, true}, {"Md", 1, true},
	}
)

func genTree(root string, n int) {
	marker := filepath.Join(root, ".bench-ok")
	want := fmt.Sprintf("v3 files=%d", n)
	if b, err := os.ReadFile(marker); err == nil && string(b) == want {
		fmt.Printf("\ntree: %s (%d files, already made)\n", root, n)
		return
	}
	fmt.Printf("\nmaking the tree: %s (%d files)...\n", root, n)
	t0 := time.Now()
	must(os.RemoveAll(root))
	must(os.MkdirAll(root, 0o755))
	rng := rand.New(rand.NewPCG(1, 2))
	total := 0
	for _, e := range exts {
		total += e.w
	}
	pick := func() (string, bool) {
		r := rng.IntN(total)
		for _, e := range exts {
			if r < e.w {
				return e.ext, e.text
			}
			r -= e.w
		}
		return "dat", false
	}
	base := time.Date(2015, 1, 1, 0, 0, 0, 0, time.UTC)
	made := 0
	dirN := 0
	mkfile := func(dir, name string, text bool) {
		p := filepath.Join(dir, name)
		f, err := os.Create(p)
		must(err)
		if text {
			var sb strings.Builder
			for sb.Len() < 200+rng.IntN(12000) {
				sb.WriteString(words[rng.IntN(len(words))])
				if rng.IntN(12) == 0 {
					sb.WriteByte('\n')
				} else {
					sb.WriteByte(' ')
				}
			}
			b := []byte(sb.String())
			if rng.IntN(25) == 0 { // a few files are not valid UTF-8 (Latin-1, cut sequences)
				for k := 0; k < 3; k++ {
					i := rng.IntN(len(b))
					b[i] = []byte{0xE9, 0xFF, 0xC3, 0x80, 0xED}[rng.IntN(5)]
				}
			}
			_, err = f.Write(b)
		} else {
			err = f.Truncate(int64(1024 + rng.IntN(40<<20))) // sparse: a size without the bytes
		}
		must(err)
		must(f.Close())
		mt := base.Add(time.Duration(rng.IntN(10*365*24*3600)) * time.Second)
		must(os.Chtimes(p, mt, mt))
		made++
	}
	for made < n {
		top := tops[rng.IntN(len(tops))]
		depth := 1 + rng.IntN(3)
		dir := filepath.Join(root, top)
		for d := 0; d < depth; d++ {
			dir = filepath.Join(dir, fmt.Sprintf("%s %d", words[rng.IntN(len(words))], rng.IntN(40)))
		}
		dirN++
		if rng.IntN(15) == 0 { // libraries: indexed by name, never by text
			dir = filepath.Join(dir, []string{"node_modules", ".git", "build", "__pycache__"}[rng.IntN(4)], "pkg")
		}
		must(os.MkdirAll(dir, 0o755))
		k := 10 + rng.IntN(60)
		for j := 0; j < k && made < n; j++ {
			ext, text := pick()
			name := fmt.Sprintf("%s_%05d", []string{"IMG", "file", "Doc", "café", "scan", "日本"}[rng.IntN(6)], rng.IntN(100000))
			if ext != "" {
				name += "." + ext
			}
			if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
				continue
			}
			mkfile(dir, name, text)
		}
		if rng.IntN(10) == 0 {
			os.WriteFile(filepath.Join(dir, ".DS_Store"), []byte("x"), 0o644) // never indexed
			os.WriteFile(filepath.Join(dir, "._IMG_00001.jpg"), []byte("x"), 0o644)
		}
	}
	// the odd ones: hidden files, a folder never indexed, links to a file and to a folder
	os.WriteFile(filepath.Join(root, ".bashrc"), []byte("export A=1\n"), 0o644)
	must(os.MkdirAll(filepath.Join(root, ".sidecar"), 0o755))
	os.WriteFile(filepath.Join(root, ".sidecar", "ignored.txt"), []byte("no"), 0o644)
	os.Symlink(".bashrc", filepath.Join(root, "link-to-file"))
	os.Symlink("Photos", filepath.Join(root, "link-to-folder"))
	must(os.WriteFile(marker, []byte(want), 0o644))
	fmt.Printf("  %d files in %d folders, %.0fs\n", made, dirN, time.Since(t0).Seconds())
}

func genBig(root string, mb int) {
	const each = 64 // MB per file
	marker := filepath.Join(root, ".bench-ok")
	want := fmt.Sprintf("v1 mb=%d", mb)
	if b, err := os.ReadFile(marker); err == nil && string(b) == want {
		fmt.Printf("big files: %s (%d MB, already made)\n", root, mb)
		return
	}
	fmt.Printf("making the big files: %s (%d MB)...\n", root, mb)
	must(os.RemoveAll(root))
	must(os.MkdirAll(root, 0o755))
	rng := rand.NewChaCha8([32]byte{7})
	buf := make([]byte, 1<<20)
	for i := 0; i*each < mb; i++ {
		f, err := os.Create(filepath.Join(root, fmt.Sprintf("video_%03d.mp4", i)))
		must(err)
		for j := 0; j < each; j++ {
			rng.Read(buf)
			_, err := f.Write(buf)
			must(err)
		}
		must(f.Close())
	}
	must(os.WriteFile(marker, []byte(want), 0o644))
}

// ------------------------------------------------------------------ small things

func median(xs []float64) float64 {
	s := slices.Clone(xs)
	slices.Sort(s)
	if len(s) == 0 {
		return 0
	}
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

func sh(dir string, env []string, name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fail("%s %v: %v", name, args, err)
	}
}

func copyFile(from, to string) {
	in, err := os.Open(from)
	must(err)
	defer in.Close()
	out, err := os.Create(to)
	must(err)
	_, err = io.Copy(out, in)
	must(err)
	must(out.Close())
}

func findRepo() string {
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			fail("run from inside the repository")
		}
		dir = parent
	}
}

func must(err error) {
	if err != nil {
		fail("%v", err)
	}
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "bench: "+format+"\n", a...)
	os.Exit(1)
}
