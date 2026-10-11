package server

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sim186/disk-sidecar/internal/install"
)

const watchEvery = 2 * time.Second

// NewDisk is a disk that was plugged in while the server ran and has no sidecar yet.
type NewDisk struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

// watcher notices disks that appear after it started. It polls the mount folders: the same code
// works on every system, and two seconds is immediate for a person.
type watcher struct {
	mu      sync.Mutex
	known   map[string]bool // every mount seen so far (the first poll is the baseline)
	pending map[string]NewDisk
	ignored map[string]bool // "never ask again", saved on disk
	file    string
}

func newWatcher() *watcher {
	w := &watcher{known: map[string]bool{}, pending: map[string]NewDisk{}, ignored: map[string]bool{}}
	if dir, err := os.UserConfigDir(); err == nil {
		w.file = filepath.Join(dir, "disk-sidecar", "ignored.json")
		if b, err := os.ReadFile(w.file); err == nil {
			var l []string
			if json.Unmarshal(b, &l) == nil {
				for _, p := range l {
					w.ignored[p] = true
				}
			}
		}
	}
	return w
}

func (w *watcher) saveIgnored() {
	if w.file == "" {
		return
	}
	var l []string
	for p := range w.ignored {
		l = append(l, p)
	}
	sort.Strings(l)
	b, _ := json.Marshal(l)
	if os.MkdirAll(filepath.Dir(w.file), 0o755) == nil {
		os.WriteFile(w.file, b, 0o644)
	}
}

// systemMount tells the volumes that are never an external disk: the system disk and its aliases.
func systemMount(p string) bool {
	if runtime.GOOS == "windows" {
		sd := strings.ToUpper(os.Getenv("SystemDrive")) // "C:"
		return sd != "" && strings.EqualFold(strings.TrimSuffix(p, `\`), sd)
	}
	if real, err := filepath.EvalSymlinks(p); err == nil && real == "/" { // "Macintosh HD" -> /
		return true
	}
	return false
}

func (s *Server) watchOnce(w *watcher, first bool) {
	have := map[string]bool{}
	for _, d := range s.discover() {
		have[d.Root] = true
	}
	now := map[string]bool{}
	for _, p := range s.mountCandidates() {
		if strings.HasPrefix(filepath.Base(p), ".") || !dirExists(p) || systemMount(p) {
			continue
		}
		now[p] = true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for p := range now {
		isNew := !w.known[p]
		w.known[p] = true
		if first || !isNew || have[p] || w.ignored[p] {
			continue
		}
		w.pending[p] = NewDisk{Path: p, Name: filepath.Base(p)}
	}
	for p := range w.known { // unplugged: forget it, so a later plug-in asks again
		if !now[p] {
			delete(w.known, p)
			delete(w.pending, p)
		}
	}
	for p := range w.pending { // got a sidecar in the meantime (made by hand, or by another copy)
		if have[p] {
			delete(w.pending, p)
		}
	}
}

func (s *Server) watchDisks() {
	s.watchOnce(s.watch, true)
	for range time.Tick(watchEvery) {
		s.watchOnce(s.watch, false)
	}
}

func (w *watcher) list() []NewDisk {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := []NewDisk{}
	for _, d := range w.pending {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// take returns the pending disk at path; only disks we proposed can be set up from the page.
func (w *watcher) take(path string) (NewDisk, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	d, ok := w.pending[path]
	return d, ok
}

// dismiss hides the proposal; forever=true also remembers it across runs.
func (w *watcher) dismiss(path string, forever bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.pending, path)
	if forever {
		w.ignored[path] = true
		w.saveIgnored()
	}
}

// doSetup installs the sidecar on a proposed disk and scans it.
func (s *Server) doSetup(d NewDisk, job *Job) (any, error) {
	job.Phase("Installing on " + d.Name + "…")
	err := install.Run(install.Options{Disk: d.Path, Yes: true, Scan: true, Out: io.Discard, Err: io.Discard,
		Progress: func(n int) { job.Update("Scanning "+d.Name+"…", int64(n), -1) }})
	if err != nil {
		return nil, err
	}
	s.watch.dismiss(d.Path, false)
	return map[string]string{"path": d.Path, "name": d.Name}, nil
}
