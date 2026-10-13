package server

// How a server ends: on Ctrl+C, on a closed Terminal window, when asked by a newer server that wants
// its port, or by itself once no page has been open for a while. Every way out removes the page's
// copy of the catalog, which can be hundreds of megabytes; copies left by a server that was killed
// are swept by the next one.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// stop asks the server to end; it is safe to call more than once.
func (s *Server) stop() { s.quitOnce.Do(func() { close(s.quit) }) }

// seen records that someone (normally the page, which polls every few seconds) is still there.
func (s *Server) seen() { s.lastSeen.Store(int64(time.Since(s.started))) }

// busy reports whether a long action (a check, a copy, emptying the quarantine) is running.
func (s *Server) busy() bool {
	s.jt.mu.Lock()
	defer s.jt.mu.Unlock()
	for _, j := range s.jt.jobs {
		j.mu.Lock()
		running := j.state == "running"
		j.mu.Unlock()
		if running {
			return true
		}
	}
	return false
}

// watchIdle stops the server once nobody has asked it anything for cfg.IdleExit and nothing runs.
// The time is monotonic: a Mac asleep does not count.
func (s *Server) watchIdle(out io.Writer) {
	every := min(30*time.Second, max(s.cfg.IdleExit/4, 100*time.Millisecond))
	for {
		select {
		case <-s.quit:
			return
		case <-time.After(every):
		}
		quiet := time.Since(s.started) - time.Duration(s.lastSeen.Load())
		if quiet >= s.cfg.IdleExit && !s.busy() {
			fmt.Fprintf(out, "No page open for %s: Sidecar stops. Open it again with Open Sidecar.\n", s.cfg.IdleExit.Round(time.Second))
			s.stop()
			return
		}
	}
}

var (
	buildOnce sync.Once
	buildID   string
)

// build identifies this exact program: two servers with the same version string can still be
// different builds (a developer's, or a disk updated while the old one ran).
func build() string {
	buildOnce.Do(func() {
		buildID = "unknown"
		exe, err := os.Executable()
		if err != nil {
			return
		}
		f, err := os.Open(exe)
		if err != nil {
			return
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err == nil {
			buildID = hex.EncodeToString(h.Sum(nil))[:16]
		}
	})
	return buildID
}

// other is another Sidecar already serving on our port.
type other struct {
	base, token, build string
}

// findOther returns the Sidecar listening on port, or nil if what listens there is something else.
func findOther(port int) *other {
	c := &http.Client{Timeout: 3 * time.Second}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	r, err := c.Get(base + "/catalog.html")
	if err != nil {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	r.Body.Close()
	m := regexp.MustCompile(`sidecar-token" content="([0-9a-f]+)"`).FindSubmatch(b)
	if m == nil {
		return nil
	}
	o := &other{base: base, token: string(m[1])}
	req, _ := http.NewRequest("GET", base+"/api/state", nil)
	req.Header.Set("X-Sidecar-Token", o.token)
	if r, err := c.Do(req); err == nil {
		var st struct {
			Build string `json:"build"`
		}
		json.NewDecoder(r.Body).Decode(&st)
		r.Body.Close()
		o.build = st.Build // "" for servers older than this code
	}
	return o
}

// askToQuit asks the other server to stop; it refuses while it is busy with a long action.
func (o *other) askToQuit() error {
	req, _ := http.NewRequest("POST", o.base+"/api/quit", strings.NewReader("{}"))
	req.Header.Set("X-Sidecar-Token", o.token)
	req.Header.Set("Content-Type", "application/json")
	r, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		var e struct {
			Error string `json:"error"`
		}
		json.NewDecoder(r.Body).Decode(&e)
		if e.Error == "" {
			e.Error = fmt.Sprintf("it answered %d", r.StatusCode) // a server older than /api/quit
		}
		return errors.New(e.Error)
	}
	return nil
}

// tempDir makes the folder for the page's copy, with our process ID inside so that a later server
// can tell when it was left behind.
func (s *Server) tempDir() (string, error) {
	if s.tmp == "" {
		d, err := os.MkdirTemp("", "sidecar-")
		if err != nil {
			return "", err
		}
		s.tmp = d
	}
	os.MkdirAll(s.tmp, 0o700) // a sweep of old temp folders may have taken it
	os.WriteFile(filepath.Join(s.tmp, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o600)
	return s.tmp, nil
}

// sweepOldTemps removes the page copies of servers that are gone: at once when the process that made
// one no longer runs, otherwise (no process ID, or a system where that cannot be checked) after a day.
func sweepOldTemps() {
	ents, _ := os.ReadDir(os.TempDir())
	for _, e := range ents {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "sidecar-") {
			continue
		}
		dir := filepath.Join(os.TempDir(), e.Name())
		if pid, err := os.ReadFile(filepath.Join(dir, "pid")); err == nil {
			if n, err := strconv.Atoi(strings.TrimSpace(string(pid))); err == nil && n != os.Getpid() && processGone(n) {
				os.RemoveAll(dir)
				continue
			}
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 24*time.Hour {
			os.RemoveAll(dir)
		}
	}
}

// processGone reports whether no process with this ID runs; false when it cannot tell.
func processGone(pid int) bool {
	if runtime.GOOS == "windows" || pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err != nil && !errors.Is(err, syscall.EPERM)
}
