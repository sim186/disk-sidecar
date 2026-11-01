//go:build cgosqlite

package idx

import _ "github.com/mattn/go-sqlite3" // the C SQLite through cgo; build with -tags "cgosqlite sqlite_fts5"

const (
	Driver    = "sqlite3"
	BusyParam = "_busy_timeout=30000"
)
