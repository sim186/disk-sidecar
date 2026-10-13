package idx

import (
	"database/sql"
	"fmt"
)

// PageSource is one source as the page sees it in the combined index.
type PageSource struct {
	DB         string // the index it lives in
	SID        int64  // its id there
	Label      string // unique across every index
	Root, Kind string
	ScannedAt  int64
}

// BuildCombined writes to dest the index the page receives: every source of every index, with its
// files and marks, and without the extracted text (the server searches that).
func BuildCombined(srcs []PageSource, dest string) error {
	db, err := sql.Open("sqlite", FileURI(dest, "_pragma=busy_timeout(30000)"))
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(combinedDDL); err != nil {
		return err
	}
	alias := map[string]string{}
	for _, x := range srcs { // ATTACH cannot happen inside a transaction
		if _, ok := alias[x.DB]; !ok {
			a := fmt.Sprintf("d%d", len(alias))
			if _, err := db.Exec(`ATTACH DATABASE ? AS `+a, FileURI(x.DB, "mode=ro")); err != nil {
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
			x.Label, x.Root, "", x.ScannedAt, x.Kind, DiskID(x.DB))
		if err != nil {
			tx.Rollback()
			return err
		}
		nid, _ := res.LastInsertId()
		if _, err := CopySource(tx, a, x.SID, nid, false); err != nil {
			tx.Rollback()
			return err
		}
		tx.Exec(`INSERT OR IGNORE INTO marks SELECT ?, path, mark, note FROM `+a+`.marks WHERE source_id=?`, nid, x.SID)
	}
	return tx.Commit()
}

// DiskID is the permanent ID of the disk an index describes, or "".
func DiskID(dbPath string) string {
	db, err := OpenRO(dbPath)
	if err != nil {
		return ""
	}
	defer db.Close()
	var v sql.NullString
	db.QueryRow(`SELECT value FROM meta WHERE key='disk_id'`).Scan(&v)
	return v.String
}
