package server

import (
	"crypto/subtle"
	"net/http"
	"os"
	"path/filepath"

	"github.com/sim186/disk-sidecar/internal/scan"
	"github.com/sim186/disk-sidecar/internal/thumb"
)

// serveThumb answers an <img> tag, which cannot send the token header: the token travels in the
// address instead, as it never leaves this computer. Only photos on a disk plugged in are shown;
// phones are never read just to draw a preview.
func (s *Server) serveThumb(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if subtle.ConstantTimeCompare([]byte(q.Get("t")), []byte(s.token)) != 1 {
		http.Error(w, "missing token", 403)
		return
	}
	full, _, err := s.diskFile(q.Get("source"), q.Get("path"))
	if err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	if info, err := os.Stat(full); err != nil || scan.Placeholder(info) { // only in the cloud: never downloaded for a preview
		http.Error(w, "no preview", 404)
		return
	}
	cache := ""
	if d, err := os.UserCacheDir(); err == nil {
		cache = filepath.Join(d, "disk-sidecar", "thumbs")
	}
	size := thumb.Size
	if q.Get("size") == "large" {
		size = thumb.Large
	}
	b, err := thumb.GetSize(r.Context(), full, cache, size)
	if err != nil {
		if r.Context().Err() != nil {
			return // the page scrolled on
		}
		http.Error(w, "no preview", 404)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Write(b)
}
