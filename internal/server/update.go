package server

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sim186/disk-sidecar/internal/assets"
)

const (
	updateURL   = "https://api.github.com/repos/sim186/disk-sidecar/releases/latest"
	updateEvery = 24 * time.Hour
)

// Update is a newer release than the one running.
type Update struct {
	Version string `json:"version"`
	URL     string `json:"url"`
}

// updater asks GitHub, at most once a day, which release is the latest. It only reads the version
// number: nothing about the user, the disks or the files is sent. It never installs anything.
type updater struct {
	mu      sync.Mutex
	file    string
	Checked int64  `json:"checked_at"`
	Latest  string `json:"latest"`
	URL     string `json:"url"`
	Skipped string `json:"skipped"`
}

func newUpdater() *updater {
	u := &updater{}
	if dir, err := os.UserConfigDir(); err == nil {
		u.file = filepath.Join(dir, "disk-sidecar", "update.json")
		if b, err := os.ReadFile(u.file); err == nil {
			json.Unmarshal(b, u)
		}
	}
	return u
}

func (u *updater) save() {
	if u.file == "" {
		return
	}
	b, _ := json.Marshal(u)
	if os.MkdirAll(filepath.Dir(u.file), 0o755) == nil {
		os.WriteFile(u.file, b, 0o644)
	}
}

// semver reads "v1.2.3" as numbers; ok is false for anything else (pre-releases included).
func semver(v string) (n [3]int, ok bool) {
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".")
	if len(parts) != 3 {
		return n, false
	}
	for i, p := range parts {
		x, err := strconv.Atoi(p)
		if err != nil || x < 0 {
			return n, false
		}
		n[i] = x
	}
	return n, true
}

func newer(latest, current string) bool {
	a, ok1 := semver(latest)
	b, ok2 := semver(current)
	if !ok1 || !ok2 {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

// check asks GitHub unless it did in the last day. Any failure (offline, rate limit) is silent.
func (u *updater) check(url string) {
	u.mu.Lock()
	fresh := time.Since(time.Unix(u.Checked, 0)) < updateEvery
	u.mu.Unlock()
	if fresh {
		return
	}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "disk-sidecar/"+assets.Version)
	resp, err := (&http.Client{Timeout: 4 * time.Second}).Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var rel struct {
		Tag string `json:"tag_name"`
		URL string `json:"html_url"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel) != nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.Checked, u.Latest, u.URL = time.Now().Unix(), strings.TrimPrefix(rel.Tag, "v"), rel.URL
	u.save()
}

// available is the newer release to tell the user about, if any.
func (u *updater) available() *Update {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.Latest == "" || u.Latest == u.Skipped || !newer(u.Latest, assets.Version) {
		return nil
	}
	return &Update{Version: u.Latest, URL: u.URL}
}

func (u *updater) skip(version string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.Skipped = version
	u.save()
}

func (s *Server) checkUpdates() {
	if s.cfg.NoUpdateCheck || os.Getenv("SIDECAR_NO_UPDATE_CHECK") != "" {
		return
	}
	url := updateURL
	if v := os.Getenv("SIDECAR_UPDATE_URL"); v != "" { // for tests
		url = v
	}
	for {
		s.upd.check(url)
		time.Sleep(time.Hour) // a long-running server notices a release published meanwhile
	}
}
