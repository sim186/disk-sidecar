package server

import (
	"crypto/subtle"
	"net/http"
	"os"
	"path/filepath"

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
	cache := ""
	if d, err := os.UserCacheDir(); err == nil {
		cache = filepath.Join(d, "disk-sidecar", "thumbs")
	}
	b, err := thumb.Get(r.Context(), full, cache)
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
