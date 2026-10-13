package tests

// The performance harness. It builds a big fake disk (sparse files, so it takes little real space),
// installs Sidecar on it, serves it and times what a person waits for: the page loading, a click on
// Keep / Not sure / Let go, the page refreshing after a change, searching, and the check at start.
// The page itself is driven in headless Chrome by perf/page.mjs (skipped when Node or Chrome is missing).
//
//	SIDECAR_PERF=1 go test ./tests -run TestPerf -v                   (100,000 files)
//	SIDECAR_PERF=1 SIDECAR_PERF_FILES=300000 go test ./tests -run TestPerf -v -timeout 30m
//
// Every measure has a budget; going over it fails the test. The time budgets are about five to ten
// times what the code does on an Apple silicon Mac, so they catch an operation that became slower in
// kind (a full scan per click, a rebuild per mark), not noise; SIDECAR_PERF_SLACK=2 doubles them on
// a slow machine. The counters (SQL per click, full scans) are exact and never get slack: a click on
// Keep / Let go must run no SQL at all, and drawing a folder must not read the whole disk.
// SIDECAR_PERF_OUT=file.json also writes every measure to a file, to compare two runs.

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sim186/disk-sidecar/internal/idx"
)

// budgets in milliseconds, for 100,000 files; operations that grow with the disk scale with it.
type budget struct {
	ms     float64
	scales bool // grows with the number of files
}

var perfBudgets = map[string]budget{
	// server, measured over HTTP
	"server.index_cold":         {2000, true}, // first download of the page's copy: built from the disk's index
	"server.index_warm":         {100, true},  // the same copy again, nothing changed
	"server.mark_p50":           {30, false},  // one click on Keep / Let go, saved on the disk
	"server.mark_max":           {200, false}, //
	"server.marks_bulk_300":     {150, false}, // 300 answers at once
	"server.index_after_mark":   {100, true},  // the copy after a mark: patched, not rebuilt
	"server.state":              {50, false},  // what the page asks at every load
	"server.search_text":        {200, true},  // searching inside files
	"server.verify_unchanged":   {5000, true}, // the check at start, nothing changed by hand
	"server.mark_during_verify": {500, false}, // slowest click while the check rewrites the index
	// the page in headless Chrome
	"page.load":                 {3000, true}, // from navigation to the first drawing, the copy already built
	"page.render_map_root":      {300, true},
	"page.render_map_folder":    {300, true},
	"page.click_mark_root":      {100, false}, // the click on Let go at the top level
	"page.click_mark_folder":    {100, false}, // the same in a folder
	"page.click_mark_files":     {100, false}, // in the list of files
	"page.view_files":           {600, true},
	"page.view_photos":          {300, true},
	"page.ctx_mark_photo":       {100, false}, // right-click a photo, Let go
	"page.bulk_mark_300":        {150, false}, // select all 300 files shown, Let go
	"page.tab_dups":             {1000, true},
	"page.tab_marks":            {400, false}, // the list of answers, about 300 of them
	"page.search_name":          {300, true},
	"page.reload_after_mark":    {1000, true}, // the page reloading its copy after a change
	"page.sql_per_click_max":    {0, false},   // not milliseconds: SQL statements one click may run
	"page.fullscans_click_max":  {0, false},   // not milliseconds: full scans of files one click may run
	"page.fullscans_folder_max": {0, false},   // not milliseconds: full scans of the disk to draw one folder
}

func TestPerf(t *testing.T) {
	if os.Getenv("SIDECAR_PERF") == "" {
		t.Skip("performance harness: run with SIDECAR_PERF=1 (see tests/perf_test.go)")
	}
	n := 100_000
	if v, err := strconv.Atoi(os.Getenv("SIDECAR_PERF_FILES")); err == nil && v > 0 {
		n = v
	}
	slack := 1.0
	if v, err := strconv.ParseFloat(os.Getenv("SIDECAR_PERF_SLACK"), 64); err == nil && v > 0 {
		slack = v
	}
	res := map[string]float64{}
	var resMu sync.Mutex
	put := func(k string, v float64) { resMu.Lock(); res[k] = v; resMu.Unlock() }

	tmp := t.TempDir()
	vol := filepath.Join(tmp, "Volumes")
	disk := filepath.Join(vol, "Big")
	t0 := time.Now()
	paths := makeFakeDisk(t, disk, n)
	t.Logf("fake disk: %d files in %.1fs", len(paths), time.Since(t0).Seconds())

	t0 = time.Now()
	mustRun(t, "install", disk, "--yes", "--label", "Big")
	put("info.scan_initial", ms(time.Since(t0)))
	db := disk + "/.sidecar/index.db"
	addDuplicates(t, db)

	s := startPerfServer(t, vol)

	// ------------------------------------------------ server
	put("server.index_cold", timeIt(func() { s.fetchIndex() }))
	put("server.index_warm", timeIt(func() { s.fetchIndex() }))
	_, st := s.call("/api/state")
	put("server.state", timeIt(func() { s.call("/api/state") }))
	_ = st

	var lat []float64
	r := rand.New(rand.NewSource(7))
	for i := 0; i < 40; i++ {
		p := paths[r.Intn(len(paths))]
		mark := []string{"keep", "delete", "review"}[i%3]
		lat = append(lat, timeIt(func() {
			s.mustCall("/api/marks", map[string]any{"items": []map[string]any{{"source": "Big", "path": p, "mark": mark}}})
		}))
	}
	put("server.mark_p50", pct(lat, 50))
	put("server.mark_max", pct(lat, 100))
	var bulk []map[string]any
	for i := 0; i < 300; i++ {
		bulk = append(bulk, map[string]any{"source": "Big", "path": paths[(i*97)%len(paths)], "mark": "delete"})
	}
	put("server.marks_bulk_300", timeIt(func() { s.mustCall("/api/marks", map[string]any{"items": bulk}) }))
	put("server.index_after_mark", timeIt(func() { s.fetchIndex() }))
	put("server.search_text", timeIt(func() { s.call("/api/search?q=invoice&kind=all&source=") }))

	// ------------------------------------------------ the page
	if out, ok := runPagePerf(t, s); ok {
		for k, v := range out {
			put(k, v)
		}
	}

	// ------------------------------------------------ the check at start, then with changes by hand
	put("server.verify_unchanged", timeIt(func() { s.mustJob("/api/verify", map[string]any{}) }))
	// a tenth of the files modified by hand: the check rewrites them while the person keeps clicking
	later := time.Now().Add(time.Hour)
	for i := 0; i < len(paths); i += 10 {
		os.Chtimes(filepath.Join(disk, paths[i]), later, later)
	}
	code, j := s.call("/api/verify", map[string]any{})
	if code != 202 {
		t.Fatalf("verify not started: %v", j)
	}
	var worst float64
	clicks := 0
	for {
		p := paths[r.Intn(len(paths))]
		worst = max(worst, timeIt(func() {
			s.mustCall("/api/marks", map[string]any{"items": []map[string]any{{"source": "Big", "path": p, "mark": "review"}}})
		}))
		clicks++
		_, v := s.call("/api/job?id=" + j["job"].(string))
		if v["state"] != "running" {
			if v["state"] != "done" {
				t.Fatalf("verify: %v", v)
			}
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	put("server.mark_during_verify", worst)
	t.Logf("%d clicks during the check", clicks)

	report(t, n, slack, res)
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func timeIt(f func()) float64 { t0 := time.Now(); f(); return ms(time.Since(t0)) }

func pct(v []float64, p int) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	i := (len(s) - 1) * p / 100
	return s[i]
}

func report(t *testing.T, n int, slack float64, res map[string]float64) {
	scale := max(1, float64(n)/100_000)
	var keys []string
	for k := range res {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "\n%-30s %12s %12s\n", fmt.Sprintf("measure (%d files)", n), "value", "budget")
	var over []string
	for _, k := range keys {
		v := res[k]
		bd, ok := perfBudgets[k]
		lim := ""
		if ok {
			l := bd.ms * slack
			if bd.scales {
				l *= scale
			}
			if strings.Contains(k, "sql_") || strings.Contains(k, "fullscans") {
				l = bd.ms // counts do not get slack
			}
			lim = fmt.Sprintf("%.0f", l)
			if v > l {
				over = append(over, fmt.Sprintf("%s: %.1f > %s", k, v, lim))
				lim += "  OVER"
			}
		}
		fmt.Fprintf(&b, "%-30s %12.1f %12s\n", k, v, lim)
	}
	t.Log(b.String())
	if p := os.Getenv("SIDECAR_PERF_OUT"); p != "" {
		data, _ := json.MarshalIndent(map[string]any{"files": n, "goos": runtime.GOOS, "goarch": runtime.GOARCH,
			"at": time.Now().Format(time.RFC3339), "results": res}, "", "  ")
		os.WriteFile(p, data, 0o644)
	}
	for k := range perfBudgets {
		if _, ok := res[k]; !ok && !strings.HasPrefix(k, "page.") {
			t.Errorf("measure %s missing", k)
		}
	}
	if len(over) > 0 {
		t.Errorf("over budget:\n  %s", strings.Join(over, "\n  "))
	}
}

// ---------------------------------------------------------------- the fake disk

// makeFakeDisk writes n files shaped like a real photo and backup disk: deep photo folders by year
// and event, music by artist and album, documents with text inside, code with node_modules, videos,
// and a backup folder that copies part of the photos. Sizes are real but the files are sparse.
func makeFakeDisk(t *testing.T, root string, n int) []string {
	r := rand.New(rand.NewSource(1))
	var paths []string
	add := func(p string, size int64) {
		full := filepath.Join(root, p)
		f, err := os.Create(full)
		if err != nil {
			os.MkdirAll(filepath.Dir(full), 0o755)
			if f, err = os.Create(full); err != nil {
				t.Fatal(err)
			}
		}
		f.Truncate(size)
		f.Close()
		paths = append(paths, p)
	}
	words := strings.Fields("invoice contract holiday budget report meeting garden recipe tax insurance letter school travel music project draft")
	text := func(p string, k int) {
		full := filepath.Join(root, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		var b strings.Builder
		for i := 0; i < 80; i++ {
			b.WriteString(words[(k*7+i*3)%len(words)] + " ")
		}
		if err := os.WriteFile(full, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	share := func(f float64) int { return int(float64(n) * f) }
	var photos []string
	for i := 0; i < share(0.50); i++ { // 50 photos per event, 12 events per year
		ev, k := i/50, i%50
		year := 2004 + ev/12
		ext, size := "jpg", int64(2_000_000+r.Intn(6_000_000))
		if k%10 == 9 {
			ext, size = "dng", int64(20_000_000+r.Intn(10_000_000))
		}
		p := fmt.Sprintf("Photos/%d/%d-%02d-%02d Event %d/IMG_%05d.%s", year, year, ev%12+1, 1+ev%28, ev, i, ext)
		add(p, size)
		photos = append(photos, p)
	}
	for i := 0; i < share(0.10) && i < len(photos); i++ { // copies: same name and size under Backups
		p := photos[i*len(photos)/max(1, share(0.10))]
		st, _ := os.Stat(filepath.Join(root, p))
		add("Backups/Old Mac/"+p, st.Size())
	}
	for i := 0; i < share(0.15); i++ {
		add(fmt.Sprintf("Music/Artist %d/Album %d/%02d Track.mp3", i/120, i/12, i%12+1), int64(3_000_000+r.Intn(7_000_000)))
	}
	for i := 0; i < share(0.10); i++ {
		dir := fmt.Sprintf("Documents/%s/%d/Folder %d", []string{"Work", "Personal", "Taxes"}[i%3], 2010+i%15, i/40)
		if i%10 == 0 && i < 30_000 {
			text(fmt.Sprintf("%s/note %s %d.txt", dir, words[i%len(words)], i), i)
		} else {
			add(fmt.Sprintf("%s/%s %d.%s", dir, words[i%len(words)], i, []string{"pdf", "docx", "xlsx", "docx", "xlsx", "odt", "pages", "docx", "xlsx", "key"}[i%10]), int64(50_000+r.Intn(2_000_000)))
		}
	}
	for i := 0; i < share(0.12); i++ {
		add(fmt.Sprintf("Code/project%d/node_modules/pkg%d/lib/sub%d/file%d.js", i/2000, i/50, i/10%5, i), int64(1_000+r.Intn(50_000)))
	}
	for i := 0; i < share(0.03); i++ {
		add(fmt.Sprintf("Video/%d/clip %d.mp4", 2010+i%15, i), int64(100_000_000+r.Int63n(2_000_000_000)))
	}
	for len(paths) < n {
		add(fmt.Sprintf("Downloads/file %d.%s", len(paths), []string{"zip", "dmg", "pdf", "png"}[len(paths)%4]), int64(10_000+r.Intn(50_000_000)))
	}
	return paths
}

// addDuplicates gives the copies under Backups the same hash as their originals, as "sidecar hash" would.
func addDuplicates(t *testing.T, db string) {
	d, err := idx.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(`UPDATE files SET sha256 = printf('%064x', size) WHERE path LIKE 'Photos/%' OR path LIKE 'Backups/%'`); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------- the server

type perfSrv struct {
	*srv
}

func startPerfServer(t *testing.T, vol string) *perfSrv {
	s := &srv{t: t, vol: vol, port: freePort(t)}
	cmd := exec.Command(bin, "serve", "--port", fmt.Sprint(s.port), "--no-open", "--no-update-check", "--no-default-mounts",
		"--no-verify", "--no-watch", "--mounts", vol)
	cmd.Env = append(os.Environ(), "HOME="+filepath.Dir(vol), "USERPROFILE="+filepath.Dir(vol))
	cmd.Stdout, cmd.Stderr = io.Discard, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopServer(cmd) })
	s.base = fmt.Sprintf("http://127.0.0.1:%d", s.port)
	var html string
	for i := 0; i < 100; i++ {
		if r, err := http.Get(s.base + "/catalog.html"); err == nil {
			b, _ := io.ReadAll(r.Body)
			r.Body.Close()
			html = string(b)
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	m := regexp.MustCompile(`sidecar-token" content="([^"]+)`).FindStringSubmatch(html)
	if m == nil {
		t.Fatal("server did not start")
	}
	s.token = m[1]
	return &perfSrv{s}
}

func (s *perfSrv) fetchIndex() {
	r, err := http.Get(s.base + "/index.db")
	if err != nil {
		s.t.Fatal(err)
	}
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	if r.StatusCode != 200 {
		s.t.Fatalf("index.db: %d", r.StatusCode)
	}
}

func (s *perfSrv) mustCall(path string, body any) map[string]any {
	code, out := s.call(path, body)
	if code != 200 {
		s.t.Fatalf("%s: %d %v", path, code, out)
	}
	return out
}

func (s *perfSrv) mustJob(path string, body any) map[string]any {
	state, res, e := s.job(path, body)
	if state != "done" {
		s.t.Fatalf("%s: %s %s", path, state, e)
	}
	return res
}

// ---------------------------------------------------------------- the page

func findChrome() string {
	if p := os.Getenv("SIDECAR_CHROME"); p != "" {
		return p
	}
	for _, p := range []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
		"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
	} {
		if exists(p) {
			return p
		}
	}
	for _, n := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome"} {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	return ""
}

// runPagePerf drives the page in headless Chrome with perf/page.mjs and returns its measures.
func runPagePerf(t *testing.T, s *perfSrv) (map[string]float64, bool) {
	node, err := exec.LookPath("node")
	chrome := findChrome()
	if err != nil || chrome == "" {
		t.Log("page measures skipped: needs node (22 or later) and Chrome or Chromium (SIDECAR_CHROME=path)")
		return nil, false
	}
	out := filepath.Join(t.TempDir(), "page.json")
	cmd := exec.Command(node, "perf/page.mjs", "--url", s.base+"/catalog.html", "--chrome", chrome, "--out", out)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("page measures failed: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	res := map[string]float64{}
	if err := json.Unmarshal(data, &res); err != nil {
		t.Fatal(err)
	}
	return res, true
}
