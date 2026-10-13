package tests

// How servers end: a second start of the same program opens the running one, a newer build takes
// over the port, a closed Terminal window and an idle server end cleanly, and every way out removes
// the page's copy of the catalog from the temporary folder (copies of servers that were killed are
// swept by the next one).

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type lifeSrv struct {
	cmd  *exec.Cmd
	out  *bytes.Buffer
	done chan struct{}
}

func startLife(t *testing.T, exe, vol string, port int, extra ...string) *lifeSrv {
	args := append([]string{"serve", "--port", fmt.Sprint(port), "--no-open", "--no-update-check", "--no-default-mounts",
		"--no-verify", "--no-watch", "--mounts", vol}, extra...)
	l := &lifeSrv{cmd: exec.Command(exe, args...), out: &bytes.Buffer{}, done: make(chan struct{})}
	l.cmd.Stdout, l.cmd.Stderr = l.out, l.out
	if err := l.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { l.cmd.Wait(); close(l.done) }()
	t.Cleanup(func() {
		if !l.ended(0) {
			l.cmd.Process.Kill()
			<-l.done
		}
	})
	return l
}

func (l *lifeSrv) ended(wait time.Duration) bool {
	select {
	case <-l.done:
		return true
	case <-time.After(wait):
		return false
	}
}

func waitUp(t *testing.T, port int) {
	for i := 0; i < 100; i++ {
		if r, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/catalog.html", port)); err == nil {
			r.Body.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("server did not start")
}

// pageCopy asks for the page's copy of the catalog and returns the temporary folder it lives in.
func pageCopy(t *testing.T, port, pid int) string {
	r, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/index.db", port))
	if err != nil || r.StatusCode != 200 {
		t.Fatalf("index.db: %v", err)
	}
	r.Body.Close()
	ents, _ := os.ReadDir(os.TempDir())
	for _, e := range ents {
		d := filepath.Join(os.TempDir(), e.Name())
		if b, err := os.ReadFile(filepath.Join(d, "pid")); err == nil && strings.TrimSpace(string(b)) == strconv.Itoa(pid) {
			return d
		}
	}
	t.Fatalf("no temporary folder of process %d", pid)
	return ""
}

func TestServerLifecycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signals and process IDs are checked on Mac and Linux")
	}
	tmp := tempDir(t)
	vol := filepath.Join(tmp, "Volumes")
	write(t, vol+"/D/a.txt", "hello")
	mustRun(t, "install", vol+"/D", "--yes", "--label", "D")
	// another build of the same version, as when a disk is updated while its server runs
	newer := filepath.Join(tmp, "sidecar-newer")
	if out, err := exec.Command("go", "build", "-ldflags=-buildid=newer", "-o", newer, "../cmd/sidecar").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	port := freePort(t)

	a := startLife(t, bin, vol, port, "--idle-exit", "0")
	waitUp(t, port)
	dirA := pageCopy(t, port, a.cmd.Process.Pid)

	// the same program started again opens the running one and ends
	again := startLife(t, bin, vol, port)
	if !again.ended(10*time.Second) || !strings.Contains(again.out.String(), "already running") {
		t.Fatalf("second start: %s", again.out)
	}
	if a.ended(0) {
		t.Fatal("the running server stopped")
	}

	// a newer build asks the old one to make way
	b := startLife(t, newer, vol, port, "--idle-exit", "0")
	if !a.ended(10 * time.Second) {
		t.Fatalf("the old server did not stop: %s", b.out)
	}
	if exists(dirA) {
		t.Fatal("the old server left its copy of the catalog behind")
	}
	waitUp(t, port)
	if b.ended(0) {
		t.Fatalf("the newer server did not take over: %s", b.out)
	}
	dirB := pageCopy(t, port, b.cmd.Process.Pid)

	// a Terminal window closed sends SIGHUP
	b.cmd.Process.Signal(syscall.SIGHUP)
	if !b.ended(10 * time.Second) {
		t.Fatal("SIGHUP did not stop the server")
	}
	if exists(dirB) {
		t.Fatal("SIGHUP left the copy of the catalog behind")
	}

	// a copy left by a server that was killed goes at the next start
	left := filepath.Join(os.TempDir(), "sidecar-lifetest-"+strconv.Itoa(os.Getpid()))
	os.MkdirAll(left, 0o700)
	os.WriteFile(filepath.Join(left, "pid"), []byte(strconv.Itoa(a.cmd.Process.Pid)), 0o600) // a has ended
	t.Cleanup(func() { os.RemoveAll(left) })

	// unused, it stops by itself
	c := startLife(t, bin, vol, port, "--idle-exit", "1s")
	if !c.ended(15 * time.Second) {
		t.Fatal("the idle server did not stop")
	}
	if !strings.Contains(c.out.String(), "No page open") {
		t.Fatalf("idle exit: %s", c.out)
	}
	if exists(left) {
		t.Fatal("the copy of a killed server was not swept")
	}
}
