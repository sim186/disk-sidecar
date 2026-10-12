// Package server is the local web server: it serves the page, keeps the indexes of the
// plugged-in disks fresh, saves marks and runs the quarantine.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sim186/disk-sidecar/internal/assets"
	"github.com/sim186/disk-sidecar/internal/phone"
)

// Config of the server.
type Config struct {
	Port            int
	HomeDB          string   // use this index as the home disk (development); default: the disk the program is on
	Mounts          []string // extra folders that contain mounted disks
	NoDefaultMounts bool
	NoOpen          bool
	NoVerify        bool
	NoWatch         bool // do not offer to set up disks plugged in while running
	NoUpdateCheck   bool // do not ask GitHub whether a newer release exists
}

// Server holds the state of a running server.
type Server struct {
	cfg        Config
	token      string
	gen        atomic.Int64 // bumped after every change we make, to refresh the page's copy
	opMu       sync.Mutex   // one writing action at a time
	jt         jobTable
	startupJob string
	lastVerify atomic.Pointer[map[string]any]
	idxMu      sync.Mutex
	cacheSig   string
	cachePath  string
	tmp        string
	watch      *watcher
	upd        *updater
}

// Run starts the server and blocks until interrupted.
func Run(cfg Config, out io.Writer) error {
	s := &Server{cfg: cfg, token: newToken(24), jt: jobTable{jobs: map[string]*Job{}}, watch: newWatcher(), upd: newUpdater()}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.Port))
	if err != nil {
		return fmt.Errorf("cannot listen on port %d: %w (is Sidecar already running?)", cfg.Port, err)
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/catalog.html", cfg.Port)
	srcs, disks := s.listSources()
	fmt.Fprintf(out, "Sidecar %s: %s\n", assets.Version, url)
	for _, d := range disks {
		n := 0
		for _, x := range srcs {
			if x.DB == d.DB {
				n++
			}
		}
		fmt.Fprintf(out, "  disk: %s  (%d sources)\n", d.Root, n)
	}
	if len(disks) == 0 {
		fmt.Fprintln(out, "  no index found: create one with  sidecar scan <disk> --db <disk>/.sidecar/index.db --source <name>")
	}
	fmt.Fprintln(out, "Press Ctrl+C to stop.")
	if len(disks) > 0 && !cfg.NoVerify {
		if j, err := s.startJob(s.doVerify2); err == nil {
			s.startupJob = j.id
		}
	}
	go s.checkUpdates()
	if !cfg.NoWatch {
		go s.watchDisks()
	}
	if !cfg.NoOpen {
		time.AfterFunc(800*time.Millisecond, func() { openBrowser(url) })
	}
	srv := &http.Server{Handler: s}
	idle := make(chan struct{})
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt)
		<-c
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
		close(idle)
	}()
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	<-idle
	if s.tmp != "" {
		os.RemoveAll(s.tmp)
	}
	return nil
}

func (s *Server) doVerify2(job *Job) (any, error) { return s.doVerify(job) }

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	cmd.Start()
}

func (s *Server) hostOK(r *http.Request) bool {
	return r.Host == fmt.Sprintf("127.0.0.1:%d", s.cfg.Port) || r.Host == fmt.Sprintf("localhost:%d", s.cfg.Port)
}

func (s *Server) authorised(r *http.Request) bool {
	return s.hostOK(r) && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Sidecar-Token")), []byte(s.token)) == 1
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		code, b = 500, []byte(`{"error":"internal error"}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	w.Write(b)
}

func fail(w http.ResponseWriter, err error) {
	if r, ok := err.(*Refused); ok {
		writeJSON(w, 409, map[string]string{"error": r.Msg})
		return
	}
	writeJSON(w, 500, map[string]string{"error": err.Error()})
}

var webVendor = func() http.Handler {
	sub, _ := fs.Sub(assets.Web(), "vendor")
	return http.StripPrefix("/vendor/", http.FileServer(http.FS(sub)))
}()

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.hostOK(r) {
		writeJSON(w, 403, map[string]string{"error": "host not allowed"})
		return
	}
	p := r.URL.Path
	if r.Method == http.MethodPost {
		s.post(w, r, p)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", 405)
		return
	}
	switch {
	case p == "/api/thumb":
		s.serveThumb(w, r)
	case p == "/api/job":
		if !s.authorised(r) {
			writeJSON(w, 403, map[string]string{"error": "missing token"})
			return
		}
		if j := s.job(r.URL.Query().Get("id")); j != nil {
			writeJSON(w, 200, j.view())
		} else {
			writeJSON(w, 404, map[string]string{"error": "unknown operation"})
		}
	case p == "/api/search":
		if !s.authorised(r) {
			writeJSON(w, 403, map[string]string{"error": "missing token"})
			return
		}
		writeJSON(w, 200, map[string]any{"rows": s.searchText(r.URL.Query())})
	case p == "/api/newdisks":
		if !s.authorised(r) {
			writeJSON(w, 403, map[string]string{"error": "missing token"})
			return
		}
		writeJSON(w, 200, map[string]any{"disks": s.watch.list(), "update": s.upd.available()})
	case p == "/api/disks":
		if !s.authorised(r) {
			writeJSON(w, 403, map[string]string{"error": "missing token"})
			return
		}
		writeJSON(w, 200, map[string]any{"disks": s.volumes()})
	case p == "/api/phones":
		if !s.authorised(r) {
			writeJSON(w, 403, map[string]string{"error": "missing token"})
			return
		}
		writeJSON(w, 200, map[string]any{"phones": phone.Detect()})
	case p == "/api/state":
		if !s.authorised(r) {
			writeJSON(w, 403, map[string]string{"error": "missing token"})
			return
		}
		writeJSON(w, 200, s.state())
	case p == "/" || p == "/catalog.html":
		page := strings.Replace(string(assets.Page()), `<meta name="sidecar-token" content="">`,
			`<meta name="sidecar-token" content="`+s.token+`">`, 1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		io.WriteString(w, page)
	case p == "/index.db":
		f, err := s.indexFile()
		if err != nil {
			http.Error(w, err.Error(), 404)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeFile(w, r, f)
	case strings.HasPrefix(p, "/vendor/") && !strings.HasSuffix(p, "/"):
		clean := path.Clean(p)
		if !strings.HasPrefix(clean, "/vendor/") || strings.Contains(clean, "..") {
			http.NotFound(w, r)
			return
		}
		if strings.HasSuffix(clean, ".wasm") {
			w.Header().Set("Content-Type", "application/wasm")
		}
		webVendor.ServeHTTP(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) post(w http.ResponseWriter, r *http.Request, p string) {
	if !s.authorised(r) {
		writeJSON(w, 403, map[string]string{"error": "missing token or host not allowed"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 20_000_000))
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request"})
		return
	}
	bad := func(e error) { writeJSON(w, 400, map[string]string{"error": "invalid request: " + e.Error()}) }
	startJob := func(fn func(*Job) (any, error)) {
		j, err := s.startJob(fn)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 202, map[string]string{"job": j.id})
	}
	switch p {
	case "/api/marks":
		var d struct {
			Items []markItem `json:"items"`
		}
		if err := json.Unmarshal(body, &d); err != nil {
			bad(err)
			return
		}
		s.opMu.Lock()
		res, err := s.doMarks(d.Items)
		s.opMu.Unlock()
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, res)
	case "/api/restore":
		var d struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(body, &d); err != nil || d.ID == "" {
			writeJSON(w, 400, map[string]string{"error": "invalid request: missing id"})
			return
		}
		s.opMu.Lock()
		res, err := s.doRestore(d.ID)
		s.opMu.Unlock()
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, res)
	case "/api/quarantine":
		var d struct {
			Items []qItem `json:"items"`
		}
		if err := json.Unmarshal(body, &d); err != nil {
			bad(err)
			return
		}
		startJob(func(j *Job) (any, error) { return s.doQuarantine(d.Items, j) })
	case "/api/copy":
		var d copyReq
		if err := json.Unmarshal(body, &d); err != nil {
			bad(err)
			return
		}
		startJob(func(j *Job) (any, error) { return s.doCopy(d, j) })
	case "/api/empty":
		var d struct {
			Confirm string `json:"confirm"`
		}
		json.Unmarshal(body, &d)
		if !letGo(d.Confirm) {
			fail(w, refuse("confirmation missing"))
			return
		}
		startJob(func(j *Job) (any, error) { return s.doEmpty(d.Confirm, j) })
	case "/api/verify":
		startJob(s.doVerify)
	case "/api/reveal":
		var d struct{ Source, Path string }
		if err := json.Unmarshal(body, &d); err != nil {
			bad(err)
			return
		}
		if err := s.doReveal(d.Source, d.Path); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	case "/api/disk/setup":
		var d struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(body, &d); err != nil {
			bad(err)
			return
		}
		var vol *Volume
		for _, v := range s.volumes() { // only mounted disks can be set up from the page
			if v.Path == d.Path {
				v := v
				vol = &v
			}
		}
		if vol == nil {
			fail(w, refuse("that disk is not connected"))
			return
		}
		startJob(func(j *Job) (any, error) { return s.doSetup(*vol, j) })
	case "/api/phone/copy":
		var d copyReq
		if err := json.Unmarshal(body, &d); err != nil {
			bad(err)
			return
		}
		startJob(func(j *Job) (any, error) { return s.doPhoneCopy(d, j) })
	case "/api/phone/release":
		var d struct {
			Items   []qItem `json:"items"`
			Confirm string  `json:"confirm"`
			Dry     bool    `json:"dry"`
		}
		if err := json.Unmarshal(body, &d); err != nil {
			bad(err)
			return
		}
		startJob(func(j *Job) (any, error) { return s.doPhoneRelease(d.Items, d.Confirm, d.Dry, j) })
	case "/api/phone/scan":
		var d struct {
			Serial string `json:"serial"`
		}
		if err := json.Unmarshal(body, &d); err != nil {
			bad(err)
			return
		}
		dev, ok := phone.Find(d.Serial)
		if !ok {
			fail(w, refuse("that phone is not connected"))
			return
		}
		startJob(func(j *Job) (any, error) {
			j.Phase("Reading " + dev.Name)
			st, err := phone.Scan(dev, nil, func(n int) { j.SetDone(int64(n)) })
			if err != nil {
				return nil, refuse("%v", err)
			}
			return map[string]any{"files": st.Total, "bytes": st.Size, "name": dev.Name}, nil
		})
	case "/api/disk/ignore":
		var d struct {
			Path    string `json:"path"`
			Forever bool   `json:"forever"`
		}
		if err := json.Unmarshal(body, &d); err != nil {
			bad(err)
			return
		}
		s.watch.dismiss(d.Path, d.Forever)
		writeJSON(w, 200, map[string]bool{"ok": true})
	case "/api/update/skip":
		var d struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal(body, &d); err != nil {
			bad(err)
			return
		}
		s.upd.skip(d.Version)
		writeJSON(w, 200, map[string]bool{"ok": true})
	default:
		writeJSON(w, 404, map[string]string{"error": "unknown action"})
	}
}
