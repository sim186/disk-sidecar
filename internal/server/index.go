package server

import (
	"database/sql"
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
	sig, err := s.indexSig(cat)
	if err != nil {
		return "", err
	}
	if s.cachePath != "" && s.cacheSig == sig {
		if _, err := os.Stat(s.cachePath); err == nil {
			return s.cachePath, nil
		}
	}
	tmp, err := s.tempDir()
	if err != nil {
		return "", err
	}
	dest := filepath.Join(tmp, fmt.Sprintf("index-%d.db", time.Now().UnixNano()))
	if err := buildCombined(cat.Sources(), dest); err != nil {
		return "", err
	}
	if s.cachePath != "" {
		os.Remove(s.cachePath)
	}
	s.cachePath, s.cacheSig = dest, sig
	return dest, nil
}

// indexSig identifies the state of the disks' indexes plus our own change counter.
func (s *Server) indexSig(cat *catalog.Catalog) (string, error) {
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
	return sig.String(), nil
}

type markPatch struct {
	label, path string
	mark, note  *string
}

// cacheCurrent reports whether the cached copy matches the disks right now.
func (s *Server) cacheCurrent() bool {
	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	if s.cachePath == "" {
		return false
	}
	sig, err := s.indexSig(s.catalog())
	return err == nil && sig == s.cacheSig
}

// patchMarks applies saved marks to the cached copy, so the next page load needs no rebuild of a
// copy that can be hundreds of megabytes. On any trouble the copy is dropped and rebuilt later.
func (s *Server) patchMarks(rows []markPatch) {
	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	if s.cachePath == "" {
		return
	}
	drop := func() { os.Remove(s.cachePath); s.cachePath, s.cacheSig = "", "" }
	db, err := sql.Open("sqlite", idx.FileURI(s.cachePath, "_pragma=busy_timeout(30000)"))
	if err != nil {
		drop()
		return
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		drop()
		return
	}
	for _, r := range rows {
		var sid int64
		if err := tx.QueryRow(`SELECT id FROM sources WHERE label=?`, r.label).Scan(&sid); err != nil {
			tx.Rollback()
			drop()
			return
		}
		if err := idx.SetMark(tx, sid, r.path, r.mark, r.note); err != nil {
			tx.Rollback()
			drop()
			return
		}
	}
	if tx.Commit() != nil {
		drop()
		return
	}
	if sig, err := s.indexSig(s.catalog()); err == nil {
		s.cacheSig = sig
	} else {
		drop()
	}
}

func buildCombined(srcs []*catalog.Source, dest string) error {
	page := make([]idx.PageSource, len(srcs))
	for i, x := range srcs {
		page[i] = idx.PageSource{DB: x.DB, SID: x.SID, Label: x.Label, Root: x.Root, Kind: x.Kind, ScannedAt: x.ScannedAt}
	}
	return idx.BuildCombined(page, dest)
}
