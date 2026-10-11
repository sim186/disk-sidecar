// Package tests runs the compiled program against fake disks: scanning, duplicates across disks,
// merging, installing and updating, and the server with every rule that protects your files.
package tests

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sim186/disk-sidecar/internal/idx"
)

var bin string // the program under test

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "sidecar-bin-")
	if err != nil {
		panic(err)
	}
	bin = filepath.Join(dir, "sidecar")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if v := os.Getenv("SIDECAR_BIN"); v != "" {
		bin = v
	} else if out, err := exec.Command("go", "build", "-o", bin, "../cmd/sidecar").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "cannot build the program: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// ---------------------------------------------------------------- helpers

func run(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running %v: %v", args, err)
	}
	return o.String(), e.String(), code
}

func mustRun(t *testing.T, args ...string) string {
	t.Helper()
	o, e, code := run(t, args...)
	if code != 0 {
		t.Fatalf("%v failed (%d): %s%s", args, code, o, e)
	}
	return o
}

func write(t *testing.T, path string, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// blob makes n bytes of content that depends on seed (the same seed gives the same content).
func blob(n int, seed int) string {
	s := fmt.Sprint(seed)
	return strings.Repeat(s, n/len(s)+1)[:n]
}

func query(t *testing.T, db string, q string, args ...any) [][]any {
	t.Helper()
	d, err := idx.OpenRO(db)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	rs, err := d.Query(q, args...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer rs.Close()
	cols, _ := rs.Columns()
	var out [][]any
	for rs.Next() {
		vals := make([]any, len(cols))
		ptr := make([]any, len(cols))
		for i := range vals {
			ptr[i] = &vals[i]
		}
		rs.Scan(ptr...)
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				vals[i] = string(b)
			}
		}
		out = append(out, vals)
	}
	return out
}

func count(t *testing.T, db, q string, args ...any) int {
	t.Helper()
	r := query(t, db, q, args...)
	if len(r) == 0 {
		return 0
	}
	return int(r[0][0].(int64))
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func tempDir(t *testing.T) string { return t.TempDir() }

func discBin() string {
	n := "bin/sidecar-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		n += ".exe"
	}
	return n
}

// ---------------------------------------------------------------- scan

func TestIncrementalScanAndRules(t *testing.T) {
	tmp := tempDir(t)
	d, db := filepath.Join(tmp, "disk"), filepath.Join(tmp, "i.db")
	write(t, d+"/a/photo.jpg", blob(2000, 1))
	write(t, d+"/a/notes.txt", "hello needle world")
	write(t, d+"/._photo.jpg", "appledouble")
	write(t, d+"/.sidecar/index.db", "must be skipped")
	write(t, d+"/$RECYCLE.BIN/x.bin", "x")
	mustRun(t, "scan", d, "--db", db, "--source", "T")
	rows := query(t, db, "select path from files order by path")
	if len(rows) != 2 || rows[0][0] != "a/notes.txt" || rows[1][0] != "a/photo.jpg" {
		t.Fatalf("indexed files: %v", rows)
	}
	if k := query(t, db, "select kind from files where name='photo.jpg'"); k[0][0] != "photo" {
		t.Fatalf("kind: %v", k)
	}
	if count(t, db, "select count(*) from file_text where file_text match 'needle'") != 1 {
		t.Fatal("text not searchable")
	}
	if v := query(t, db, "select value from meta where key='disk_id'"); len(v) == 0 || v[0][0] == "" {
		t.Fatal("no disk id")
	}
	if v, _ := os.ReadFile("../internal/assets/VERSION"); query(t, db, "select value from meta where key='sidecar_version'")[0][0] != strings.TrimSpace(string(v)) {
		t.Fatal("version not recorded")
	}
	if out := mustRun(t, "scan", d, "--db", db, "--source", "T"); !strings.Contains(out, "new 0, changed 0, same 2") {
		t.Fatalf("second scan: %s", out)
	}
	write(t, d+"/a/new.txt", "new")
	time.Sleep(1100 * time.Millisecond)
	write(t, d+"/a/notes.txt", "changed content")
	os.Remove(d + "/a/photo.jpg")
	if out := mustRun(t, "scan", d, "--db", db, "--source", "T"); !strings.Contains(out, "new 1, changed 1, same 0, removed 1") {
		t.Fatalf("third scan: %s", out)
	}
}

func TestRefusesNewerSchema(t *testing.T) {
	tmp := tempDir(t)
	d, db := filepath.Join(tmp, "disk"), filepath.Join(tmp, "i.db")
	write(t, d+"/a.txt", "x")
	mustRun(t, "scan", d, "--db", db, "--source", "T")
	c, _ := idx.Open(db)
	c.Exec("update meta set value='999' where key='schema_version'")
	c.Close()
	o, e, code := run(t, "scan", d, "--db", db, "--source", "T")
	if code == 0 || !strings.Contains(o+e, "newer") {
		t.Fatalf("expected a refusal, got %d %s%s", code, o, e)
	}
}

// ---------------------------------------------------------------- hash, merge

func TestHashOnlyNeededFilesAndAlsoMatch(t *testing.T) {
	tmp := tempDir(t)
	a, b := filepath.Join(tmp, "A"), filepath.Join(tmp, "B")
	write(t, a+"/x/same.bin", blob(1_200_000, 7))
	write(t, b+"/y/copy.bin", blob(1_200_000, 7))
	write(t, a+"/x/other.bin", blob(1_300_000, 8))
	write(t, b+"/y/diff.bin", blob(1_300_000, 9)) // same size as other.bin, different content
	write(t, a+"/x/only.bin", blob(1_100_000, 3))
	adb, bdb := filepath.Join(tmp, "a.db"), filepath.Join(tmp, "b.db")
	mustRun(t, "scan", a, "--db", adb, "--source", "A")
	mustRun(t, "scan", b, "--db", bdb, "--source", "B")
	mustRun(t, "hash", "--db", adb)
	if n := count(t, adb, "select count(*) from files where sha256 is not null"); n != 0 {
		t.Fatalf("no ties inside A, yet %d files were hashed", n)
	}
	mustRun(t, "hash", "--db", adb, "--also-match", bdb)
	mustRun(t, "hash", "--db", bdb, "--also-match", adb)
	got := map[string]string{}
	for _, r := range query(t, adb, "select name, sha256 from files where sha256 is not null") {
		got[r[0].(string)] = r[1].(string)
	}
	if len(got) != 2 || got["same.bin"] == "" || got["other.bin"] == "" {
		t.Fatalf("hashed in A: %v (only.bin has a unique size and must never be read)", got)
	}
	hb := map[string]string{}
	for _, r := range query(t, bdb, "select name, sha256 from files where sha256 is not null") {
		hb[r[0].(string)] = r[1].(string)
	}
	if got["same.bin"] != hb["copy.bin"] || got["other.bin"] == hb["diff.bin"] {
		t.Fatalf("hashes: %v %v", got, hb)
	}
}

func TestMergeReplacesSameLabel(t *testing.T) {
	tmp := tempDir(t)
	a, b := filepath.Join(tmp, "A"), filepath.Join(tmp, "B")
	write(t, a+"/x/one.bin", blob(1_200_000, 1))
	write(t, b+"/y/copy.bin", blob(1_200_000, 1))
	write(t, b+"/y/two.bin", blob(1_300_000, 2))
	adb, bdb, main := filepath.Join(tmp, "a.db"), filepath.Join(tmp, "b.db"), filepath.Join(tmp, "main.db")
	mustRun(t, "scan", a, "--db", adb, "--source", "A")
	mustRun(t, "scan", b, "--db", bdb, "--source", "B")
	mustRun(t, "merge", main, bdb)
	if n := count(t, main, "select count(*) from files"); n != 2 {
		t.Fatalf("files after first merge: %d", n)
	}
	write(t, b+"/y/more.txt", "needle")
	mustRun(t, "scan", b, "--db", bdb, "--source", "B")
	mustRun(t, "merge", main, bdb)
	if count(t, main, "select count(*) from sources") != 1 || count(t, main, "select count(*) from files") != 3 {
		t.Fatal("same label must be replaced, not duplicated")
	}
	if count(t, main, "select count(*) from file_text where file_text match 'needle'") != 1 {
		t.Fatal("text not merged")
	}
	if out := mustRun(t, "merge", main, adb, "--dry-run"); !strings.Contains(out, "dry run") {
		t.Fatalf("dry run: %s", out)
	}
	if count(t, main, "select count(*) from sources") != 1 {
		t.Fatal("dry run changed something")
	}
}

// ---------------------------------------------------------------- install

func TestInstallAndUpdateKeepUserData(t *testing.T) {
	tmp := tempDir(t)
	disk := filepath.Join(tmp, "disk")
	write(t, disk+"/stuff/a.txt", "hi")
	mustRun(t, "install", disk, "--yes", "--label", "Mine")
	sc := disk + "/.sidecar"
	for _, f := range []string{"README.md", "VERSION", "open-sidecar.command", "open-sidecar.bat", "sidecar", "sidecar.cmd", discBin(), "index.db", "MANIFEST"} {
		if !exists(filepath.Join(sc, filepath.FromSlash(f))) {
			t.Fatalf("missing %s after install", f)
		}
	}
	if r := query(t, sc+"/index.db", "select label from sources"); len(r) != 1 || r[0][0] != "Mine" {
		t.Fatalf("sources: %v", r)
	}
	// simulate an older install with a file that no longer exists in the package
	write(t, sc+"/VERSION", "0.0.1\n")
	write(t, sc+"/old_tool.py", "x")
	f, _ := os.OpenFile(sc+"/MANIFEST", os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("old_tool.py\n")
	f.Close()
	write(t, sc+"/log.csv", "keep me")
	write(t, sc+"/peers/p.db", "peer")
	f, _ = os.OpenFile(sc+"/README.md", os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("tampered\n")
	f.Close()
	out := mustRun(t, "install", disk, "--yes")
	if !strings.Contains(out, "replacing 0.0.1") {
		t.Fatalf("update output: %s", out)
	}
	if exists(sc + "/old_tool.py") {
		t.Fatal("obsolete file kept")
	}
	if b, _ := os.ReadFile(sc + "/log.csv"); string(b) != "keep me" || !exists(sc+"/peers/p.db") {
		t.Fatal("user data touched")
	}
	if count(t, sc+"/index.db", "select count(*) from files") != 1 {
		t.Fatal("index changed")
	}
	if b, _ := os.ReadFile(sc + "/README.md"); strings.Contains(string(b), "tampered") {
		t.Fatal("package file not restored")
	}
	ents, _ := os.ReadDir(sc)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "._") {
			t.Fatalf("stray file %s", e.Name())
		}
	}
}

func TestInstallDryRunWritesNothing(t *testing.T) {
	disk := filepath.Join(tempDir(t), "disk")
	os.MkdirAll(disk, 0o755)
	if out := mustRun(t, "install", disk, "--dry-run"); !strings.Contains(out, "would copy") {
		t.Fatalf("dry run: %s", out)
	}
	if exists(disk + "/.sidecar") {
		t.Fatal("dry run created .sidecar")
	}
}

// ---------------------------------------------------------------- server

type srv struct {
	t           *testing.T
	base, token string
	port        int
	vol, A, B   string
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// newServer builds two plugged-in disks and a snapshot of a third computer, and serves them.
func newServer(t *testing.T) *srv {
	tmp := tempDir(t)
	s := &srv{t: t, vol: filepath.Join(tmp, "Volumes")}
	s.A, s.B = s.vol+"/DiskA", s.vol+"/DiskB"
	write(t, s.A+"/foto/shared.bin", blob(1_500_000, 1))
	write(t, s.B+"/backup/shared.bin", blob(1_500_000, 1))
	write(t, s.A+"/foto/onlyA.bin", blob(1_200_000, 2))
	write(t, s.B+"/backup/onlyB.bin", blob(1_300_000, 3))
	write(t, s.A+"/doc/note.txt", "ciao")
	write(t, s.A+"/doc/gone.txt", "bye")
	write(t, s.A+"/doc/edit.txt", "v1")
	write(t, tmp+"/pc/Foto/shared.bin", blob(1_500_000, 1))
	mustRun(t, "install", s.A, "--yes", "--label", "Disk A")
	mustRun(t, "install", s.B, "--yes", "--label", "Disk B")
	ia, ib := s.A+"/.sidecar/index.db", s.B+"/.sidecar/index.db"
	mustRun(t, "hash", "--db", ia, "--also-match", ib)
	mustRun(t, "hash", "--db", ib, "--also-match", ia)
	mustRun(t, "scan", tmp+"/pc", "--db", tmp+"/pc.db", "--source", "Laptop")
	mustRun(t, "hash", "--db", tmp+"/pc.db", "--also-match", ia)
	os.MkdirAll(s.A+"/.sidecar/peers", 0o755)
	data, _ := os.ReadFile(tmp + "/pc.db")
	os.WriteFile(s.A+"/.sidecar/peers/laptop.db", data, 0o644)
	// changes made by hand after the scan
	write(t, s.A+"/doc/new.txt", "new")
	os.Remove(s.A + "/doc/gone.txt")
	time.Sleep(1100 * time.Millisecond)
	write(t, s.A+"/doc/edit.txt", "version two!")

	s.port = freePort(t)
	cmd := exec.Command(s.A+"/.sidecar/"+discBin(), "serve", "--port", fmt.Sprint(s.port), "--no-open", "--no-default-mounts", "--mounts", s.vol)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
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
	return s
}

func (s *srv) do(method, path string, body any, token bool, host string) (int, map[string]any) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, s.base+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if token {
		req.Header.Set("X-Sidecar-Token", s.token)
	}
	if host != "" {
		req.Host = host
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	var out map[string]any
	json.Unmarshal(b, &out)
	return r.StatusCode, out
}

func (s *srv) call(path string, bodies ...any) (int, map[string]any) {
	var body any
	m := "GET"
	if len(bodies) > 0 {
		m, body = "POST", bodies[0]
	}
	return s.do(m, path, body, true, "")
}

// job starts a long action and waits for it; it returns the final state and result or error.
func (s *srv) job(path string, body any) (string, map[string]any, string) {
	code, j := s.call(path, body)
	if code != 202 {
		e, _ := j["error"].(string)
		return "rejected", nil, e
	}
	for {
		_, v := s.call("/api/job?id=" + j["job"].(string))
		if v["state"] != "running" {
			res, _ := v["result"].(map[string]any)
			e, _ := v["error"].(string)
			return v["state"].(string), res, e
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (s *srv) waitStartup() map[string]any {
	_, st := s.call("/api/state")
	id, _ := st["startup_job"].(string)
	for {
		_, v := s.call("/api/job?id=" + id)
		if v["state"] != "running" {
			return v
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (s *srv) index() string {
	r, err := http.Get(s.base + "/index.db")
	if err != nil {
		s.t.Fatal(err)
	}
	defer r.Body.Close()
	p := filepath.Join(s.t.TempDir(), "served.db")
	f, _ := os.Create(p)
	io.Copy(f, r.Body)
	f.Close()
	return p
}

func TestServerDisksFoundAndChangesByHandDetected(t *testing.T) {
	s := newServer(t)
	_, st := s.call("/api/state")
	var got []string
	for _, x := range st["sources"].([]any) {
		m := x.(map[string]any)
		got = append(got, m["label"].(string)+"/"+m["kind"].(string))
	}
	if strings.Join(got, ",") != "Disk A/disk,Disk B/disk,Laptop/snapshot" {
		t.Fatalf("sources: %v", got)
	}
	v := s.waitStartup()["result"].(map[string]any)
	if v["new"] != 1.0 || v["changed"] != 1.0 || v["removed"] != 1.0 {
		t.Fatalf("hand changes: %v", v)
	}
	db := s.index()
	if count(t, db, "select count(*) from files where name='gone.txt'") != 0 {
		t.Fatal("removed file still listed")
	}
	if count(t, db, "select count(*) from (select 1 from files where sha256 is not null group by sha256 having count(distinct source_id) = 3)") != 1 {
		t.Fatal("the same file on both disks and on the laptop was not recognised")
	}
}

func TestServerSearchesInsideFilesAndKeepsThePageIndexLean(t *testing.T) {
	s := newServer(t)
	s.waitStartup()
	code, res := s.do("GET", "/api/search?q=cia&kind=all", nil, true, "")
	rows, _ := res["rows"].([]any)
	if code != 200 || len(rows) != 1 || rows[0].(map[string]any)["path"] != "doc/note.txt" || rows[0].(map[string]any)["source"] != "Disk A" {
		t.Fatalf("content search: %d %v", code, res)
	}
	if c, _ := s.do("GET", "/api/search?q=cia", nil, false, ""); c != 403 {
		t.Fatalf("search without token: %d", c)
	}
	if count(t, s.index(), "select count(*) from sqlite_master where name='file_text'") != 0 {
		t.Fatal("the page's index must not carry the extracted text")
	}
}

func TestServerSecurityRules(t *testing.T) {
	s := newServer(t)
	s.waitStartup() // only one action runs at a time
	check := func(what string, got, want int) {
		t.Helper()
		if got != want {
			t.Errorf("%s: %d, want %d", what, got, want)
		}
	}
	c, _ := s.do("GET", "/api/state", nil, false, "")
	check("state without token", c, 403)
	c, _ = s.do("GET", "/api/state", nil, true, "evil.example")
	check("state with wrong host", c, 403)
	c, _ = s.do("POST", "/api/marks", map[string]any{"items": []any{}}, false, "")
	check("marks without token", c, 403)
	for _, p := range []string{"../x", ".sidecar/index.db"} {
		st, _, _ := s.job("/api/quarantine", map[string]any{"items": []any{map[string]string{"source": "Disk A", "path": p}}})
		if st != "error" {
			t.Errorf("path %q: %s", p, st)
		}
	}
	if st, _, _ := s.job("/api/quarantine", map[string]any{"items": []any{map[string]string{"source": "Nope", "path": "a"}}}); st != "error" {
		t.Errorf("unknown source: %s", st)
	}
	c, _ = s.call("/api/empty", map[string]any{})
	check("empty without the word", c, 409)
	c, _ = s.call("/serve.py", nil)
	check("a file that is not part of the app", c, 404)
	for _, raw := range []string{"/vendor/../x", "/vendor/%2e%2e/x", "/vendor/..%2fx", "/vendor/", "/catalog.html.bak"} {
		conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", s.port))
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(conn, "GET %s HTTP/1.0\r\nHost: 127.0.0.1:%d\r\n\r\n", raw, s.port) // raw path, not normalised
		b, _ := io.ReadAll(conn)
		conn.Close()
		if !strings.HasPrefix(string(b), "HTTP/1.0 404") && !strings.HasPrefix(string(b), "HTTP/1.1 404") {
			t.Errorf("%s: %s", raw, strings.SplitN(string(b), "\r\n", 2)[0])
		}
	}
}

func TestServerMarksFollowTheDiskThatOwnsTheFile(t *testing.T) {
	s := newServer(t)
	s.call("/api/state")
	c, _ := s.call("/api/marks", map[string]any{"items": []any{
		map[string]string{"source": "Disk B", "path": "backup/onlyB.bin", "mark": "delete"},
		map[string]string{"source": "Disk A", "path": "foto/onlyA.bin", "mark": "keep"}}})
	if c != 200 {
		t.Fatalf("marks: %d", c)
	}
	if r := query(t, s.B+"/.sidecar/index.db", "select path, mark from marks"); len(r) != 1 || r[0][0] != "backup/onlyB.bin" || r[0][1] != "delete" {
		t.Fatalf("B marks: %v", r)
	}
	if r := query(t, s.A+"/.sidecar/index.db", "select path, mark from marks"); len(r) != 1 || r[0][0] != "foto/onlyA.bin" || r[0][1] != "keep" {
		t.Fatalf("A marks: %v", r)
	}
	c, e := s.call("/api/marks", map[string]any{"items": []any{map[string]string{"source": "Laptop", "path": "Foto/shared.bin", "mark": "delete"}}})
	if c != 409 || !strings.Contains(fmt.Sprint(e["error"]), "sola lettura") {
		t.Fatalf("snapshot: %d %v", c, e)
	}
}

func TestServerQuarantineRestoreEmpty(t *testing.T) {
	s := newServer(t)
	s.waitStartup()
	item := func(src, p string) map[string]string { return map[string]string{"source": src, "path": p} }
	// refuses to take every copy of a duplicated file, across two disks
	st, _, msg := s.job("/api/quarantine", map[string]any{"items": []any{item("Disk A", "foto/shared.bin"), item("Disk B", "backup/shared.bin")}})
	if st != "error" || !strings.Contains(msg, "TUTTE le copie") {
		t.Fatalf("last copy: %s %s", st, msg)
	}
	if !exists(s.A+"/foto/shared.bin") || !exists(s.B+"/backup/shared.bin") {
		t.Fatal("a file moved although the move was refused")
	}
	// snapshots are read only
	if st, _, _ := s.job("/api/quarantine", map[string]any{"items": []any{item("Laptop", "Foto/shared.bin")}}); st != "error" {
		t.Fatalf("snapshot: %s", st)
	}
	// one copy of the duplicate plus a file with a mark: allowed, and the marks of moved items disappear
	s.call("/api/marks", map[string]any{"items": []any{map[string]string{"source": "Disk B", "path": "backup/onlyB.bin", "mark": "delete"}}})
	st, res, msg := s.job("/api/quarantine", map[string]any{"items": []any{item("Disk B", "backup/shared.bin"), item("Disk B", "backup/onlyB.bin")}})
	if st != "done" || res["moved"] != 2.0 {
		t.Fatalf("move: %s %v %s", st, res, msg)
	}
	ib := s.B + "/.sidecar/index.db"
	if exists(s.B+"/backup/onlyB.bin") || count(t, ib, "select count(*) from marks") != 0 || count(t, ib, "select count(*) from files") != 0 {
		t.Fatal("move incomplete")
	}
	_, state := s.call("/api/state")
	moves := state["quarantine"].([]any)
	if len(moves) != 2 {
		t.Fatalf("quarantine: %v", moves)
	}
	var id string
	for _, m := range moves {
		if mm := m.(map[string]any); mm["path"] == "backup/onlyB.bin" {
			id = mm["id"].(string)
		}
	}
	if c, e := s.call("/api/restore", map[string]any{"id": id}); c != 200 {
		t.Fatalf("restore: %d %v", c, e)
	}
	if !exists(s.B+"/backup/onlyB.bin") || count(t, ib, "select count(*) from files") != 1 {
		t.Fatal("restore incomplete")
	}
	if st, _, msg := s.job("/api/empty", map[string]any{"confirm": "ELIMINA"}); st != "done" {
		t.Fatalf("empty: %s %s", st, msg)
	}
	if exists(s.B+"/.sidecar/_quarantena") || !exists(s.A+"/foto/shared.bin") {
		t.Fatal("empty touched the wrong things")
	}
}

func TestServerRefusesChangedFiles(t *testing.T) {
	s := newServer(t)
	s.waitStartup()
	time.Sleep(1100 * time.Millisecond)
	write(t, s.A+"/foto/onlyA.bin", blob(1_250_000, 4)) // changed after the scan
	st, _, msg := s.job("/api/quarantine", map[string]any{"items": []any{map[string]string{"source": "Disk A", "path": "foto/onlyA.bin"}}})
	if st != "error" || !strings.Contains(msg, "modificato") {
		t.Fatalf("changed file: %s %s", st, msg)
	}
	write(t, s.A+"/doc/extra.txt", "x") // a folder that gained a file
	if st, _, _ := s.job("/api/quarantine", map[string]any{"items": []any{map[string]string{"source": "Disk A", "path": "doc"}}}); st != "error" {
		t.Fatalf("changed folder: %s", st)
	}
}

var _ = sql.ErrNoRows
