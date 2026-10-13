package server

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sim186/disk-sidecar/internal/catalog"
	"github.com/sim186/disk-sidecar/internal/idx"
)

// indexFile returns the path of the index to hand to the page: a lean, consistent copy of the
// plugged-in disks and snapshots, without the extracted text (the server searches that). It is cached until
// something changes.
func (s *Server) indexFile() (string, error) {
	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	cat := s.catalog()
	srcs := cat.Sources()
	dbs := cat.Indexes()
	sort.Strings(dbs)
	if len(dbs) == 0 {
		return "", fmt.Errorf("no index found")
	}
	var sig strings.Builder
	fmt.Fprintf(&sig, "%d", s.gen.Load())
	for _, p := range dbs {
		st, err := os.Stat(p)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&sig, "|%s:%d:%d", p, st.ModTime().UnixNano(), st.Size())
	}
	if s.cachePath != "" && s.cacheSig == sig.String() {
		if _, err := os.Stat(s.cachePath); err == nil {
			return s.cachePath, nil
		}
	}
	if s.tmp == "" {
		d, err := os.MkdirTemp("", "sidecar-")
		if err != nil {
			return "", err
		}
		s.tmp = d
	}
	dest := filepath.Join(s.tmp, fmt.Sprintf("index-%d.db", time.Now().UnixNano()))
	if err := buildCombined(srcs, dest); err != nil {
		return "", err
	}
	if s.cachePath != "" {
		os.Remove(s.cachePath)
	}
	s.cachePath, s.cacheSig = dest, sig.String()
	return dest, nil
}

func buildCombined(srcs []*catalog.Source, dest string) error {
	page := make([]idx.PageSource, len(srcs))
	for i, x := range srcs {
		page[i] = idx.PageSource{DB: x.DB, SID: x.SID, Label: x.Label, Root: x.Root, Kind: x.Kind, ScannedAt: x.ScannedAt}
	}
	return idx.BuildCombined(page, dest)
}
