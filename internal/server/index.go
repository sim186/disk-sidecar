package server

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sim186/disk-sidecar/internal/idx"
)

// indexFile returns the path of the index to hand to the page: a lean, consistent copy of the
// plugged-in disks and snapshots, without the extracted text (the server searches that). It is cached until
// something changes.
func (s *Server) indexFile() (string, error) {
	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	srcs, _ := s.listSources()
	set := map[string]bool{}
	for _, x := range srcs {
		set[x.DB] = true
	}
	var dbs []string
	for p := range set {
		dbs = append(dbs, p)
	}
	sort.Strings(dbs)
	if len(dbs) == 0 {
		return "", fmt.Errorf("nessun indice trovato")
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

func buildCombined(srcs []*Src, dest string) error {
	db, err := sql.Open("sqlite", idx.FileURI(dest, "_pragma=busy_timeout(30000)"))
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(idx.CombinedDDL); err != nil {
		return err
	}
	alias := map[string]string{}
	for _, x := range srcs { // ATTACH cannot happen inside a transaction
		if _, ok := alias[x.DB]; !ok {
			a := fmt.Sprintf("d%d", len(alias))
			if _, err := db.Exec(`ATTACH DATABASE ? AS `+a, idx.FileURI(x.DB, "mode=ro")); err != nil {
				return err
			}
			alias[x.DB] = a
		}
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	for _, x := range srcs {
		a := alias[x.DB]
		res, err := tx.Exec(`INSERT INTO sources(label, root, host, scanned_at, kind, disk_id) VALUES (?,?,?,?,?,?)`,
			x.Label, x.Root, "", x.ScannedAt, x.Kind, diskIDOf(x.DB))
		if err != nil {
			tx.Rollback()
			return err
		}
		nid, _ := res.LastInsertId()
		if _, err := tx.Exec(`INSERT INTO files(source_id, path, name, ext, size, mtime, kind, top, sha256, seen)
			SELECT ?, path, name, ext, size, mtime, kind, top, sha256, 1 FROM `+a+`.files WHERE source_id=?`, nid, x.SID); err != nil {
			tx.Rollback()
			return err
		}
		// the text stays in the disks' own indexes: the page asks the server to search it
		tx.Exec(`INSERT OR IGNORE INTO marks SELECT ?, path, mark, note FROM `+a+`.marks WHERE source_id=?`, nid, x.SID)
	}
	return tx.Commit()
}
