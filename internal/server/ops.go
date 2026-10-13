package server

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sim186/disk-sidecar/internal/assets"
	"github.com/sim186/disk-sidecar/internal/catalog"
	"github.com/sim186/disk-sidecar/internal/farewell"
	"github.com/sim186/disk-sidecar/internal/idx"
	"github.com/sim186/disk-sidecar/internal/scan"
)

// ---------------------------------------------------------------- marks

type markItem struct {
	Source string  `json:"source"`
	Path   string  `json:"path"`
	Mark   *string `json:"mark"`
	Note   *string `json:"note"`
}

func (s *Server) doMarks(items []markItem) (any, error) {
	cat := s.catalog()
	type mk struct {
		sid  int64
		rel  string
		mark *string
		note *string
	}
	groups := map[string][]mk{}
	for _, it := range items {
		src, err := cat.Get(it.Source)
		if err != nil {
			return nil, err
		}
		if src.Kind != "disk" && src.Kind != "phone" {
			return nil, refuse("%s is %s: marks must be set from its own computer", src.Label, src.ReadOnlyWhat())
		}
		rel, err := catalog.CleanRel(it.Path)
		if err != nil {
			return nil, err
		}
		if it.Mark != nil && *it.Mark != "keep" && *it.Mark != "delete" && *it.Mark != "review" {
			return nil, refuse("invalid mark: %q", *it.Mark)
		}
		groups[src.DB] = append(groups[src.DB], mk{src.SID, rel, it.Mark, it.Note})
	}
	for dbPath, rows := range groups {
		db, err := idx.Open(dbPath)
		if err != nil {
			return nil, err
		}
		tx, err := db.Begin()
		if err != nil {
			db.Close()
			return nil, err
		}
		for _, r := range rows {
			if err := idx.SetMark(tx, r.sid, r.rel, r.mark, r.note); err != nil {
				tx.Rollback()
				db.Close()
				return nil, err
			}
		}
		err = tx.Commit()
		db.Close()
		if err != nil {
			return nil, err
		}
	}
	s.gen.Add(1)
	return map[string]any{"saved": len(items)}, nil
}

// box is the farewell box of what is visible now, reporting to job (nil: silent).
func (s *Server) box(job *Job) farewell.Box {
	b := farewell.Box{Catalog: s.catalog(), Guard: s.guardPath}
	if job != nil {
		b.Progress = job
	}
	return b
}

// ---------------------------------------------------------------- verification of changes made by hand

func (s *Server) doVerify(job *Job) (any, error) { return s.verify(job, true) }

func (s *Server) verify(job *Job, withHome bool) (any, error) {
	srcs := s.catalog().Sources()
	host, _ := os.Hostname()
	totals := map[string]any{"new": 0, "changed": 0, "removed": 0, "errors": 0, "disks": 0, "secs": 0.0,
		"at": time.Now().Unix(), "details": []map[string]any{}}
	var nNew, nCh, nRm, nErr, nDisks int
	var secs float64
	details := []map[string]any{}
	for _, src := range srcs {
		if !src.Reachable() {
			continue
		}
		home := scan.IsHome(src.Root)
		if home && !withHome {
			continue
		}
		hint := int64(0)
		if db, err := idx.OpenRO(src.DB); err == nil {
			db.QueryRow(`SELECT COUNT(*) FROM files WHERE source_id=?`, src.SID).Scan(&hint)
			db.Close()
		}
		if job != nil {
			job.Update("Checking "+src.Label+"…", 0, hint)
		}
		st, err := scan.Run(scan.Options{Root: src.Root, DB: src.DB, Label: src.Raw, Host: host, HashMin: -1, Text: !home,
			Progress: func(n int) {
				if job != nil {
					job.SetDone(int64(n))
				}
			}})
		if err != nil {
			return nil, err
		}
		if db, err := idx.Open(src.DB); err == nil {
			idx.LogScan(db, src.SID, "verify", st.New, st.Changed, st.Removed, st.Errors, st.Secs)
			db.Close()
		}
		nNew, nCh, nRm, nErr, nDisks, secs = nNew+st.New, nCh+st.Changed, nRm+st.Removed, nErr+st.Errors, nDisks+1, secs+st.Secs
		if st.New+st.Changed+st.Removed > 0 {
			details = append(details, map[string]any{"source": src.Label, "new": st.New, "changed": st.Changed, "removed": st.Removed})
		}
	}
	totals["new"], totals["changed"], totals["removed"], totals["errors"] = nNew, nCh, nRm, nErr
	totals["disks"], totals["secs"], totals["details"] = nDisks, secs, details
	s.lastVerify.Store(&totals)
	return totals, nil
}

// ---------------------------------------------------------------- state for the page

func (s *Server) state() map[string]any {
	cat := s.catalog()
	srcs, disks := cat.Sources(), cat.Disks()
	var warnings []string
	ids := map[string]string{}
	for _, d := range disks {
		id := idx.DiskID(d.DB)
		if prev, ok := ids[id]; ok && id != "" {
			warnings = append(warnings, fmt.Sprintf("Two disks have the same ID (%s and %s): one is a clone of the other.", prev, d.Root))
		}
		ids[id] = d.Root
	}
	type sv struct {
		Label     string `json:"label"`
		Kind      string `json:"kind"`
		Reachable bool   `json:"reachable"`
		ScannedAt int64  `json:"scanned_at"`
	}
	out := []sv{}
	for _, x := range srcs {
		out = append(out, sv{x.Label, x.Kind, x.Reachable(), x.ScannedAt})
	}
	var startup any
	if s.startupJob != "" {
		startup = s.startupJob
	}
	var verify any
	if v := s.lastVerify.Load(); v != nil {
		verify = *v
	}
	if warnings == nil {
		warnings = []string{}
	}
	return map[string]any{"actions": true, "version": assets.Version, "quarantine": farewell.Box{Catalog: cat}.Contents(),
		"sources": out, "startup_job": startup, "verify": verify, "warnings": warnings}
}

// ---------------------------------------------------------------- search inside files

type hit struct {
	Source string `json:"source"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Kind   string `json:"kind"`
	How    string `json:"how"`
}

// searchText looks for words inside the extracted text of every disk and snapshot. The page does
// not carry that text (it can be gigabytes); it asks here.
func (s *Server) searchText(q map[string][]string) []hit {
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	var terms []string
	for _, t := range strings.Fields(get("q")) {
		terms = append(terms, `"`+strings.ReplaceAll(t, `"`, `""`)+`"*`)
	}
	out := []hit{}
	if len(terms) == 0 {
		return out
	}
	cat := s.catalog()
	for _, dbPath := range cat.Indexes() {
		db, err := idx.OpenRO(dbPath)
		if err != nil {
			continue
		}
		for _, x := range cat.In(dbPath) {
			if want := get("source"); want != "" && want != x.Label {
				continue
			}
			sqlq := `SELECT f.path, f.size, f.kind FROM file_text t JOIN files f ON f.id = t.rowid WHERE file_text MATCH ? AND f.source_id = ?`
			args := []any{strings.Join(terms, " "), x.SID}
			if k := get("kind"); k != "" && k != "all" {
				sqlq += ` AND f.kind = ?`
				args = append(args, k)
			}
			rows, err := db.Query(sqlq+` LIMIT 150`, args...)
			if err != nil {
				continue
			}
			for rows.Next() {
				h := hit{Source: x.Label, How: "content"}
				if rows.Scan(&h.Path, &h.Size, &h.Kind) == nil {
					out = append(out, h)
				}
			}
			rows.Close()
		}
		db.Close()
	}
	if len(out) > 150 {
		out = out[:150]
	}
	return out
}
