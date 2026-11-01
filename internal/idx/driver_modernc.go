//go:build !cgosqlite

package idx

import _ "modernc.org/sqlite" // pure Go SQLite, with FTS5: no C compiler, builds for every system

// Driver is the database/sql driver name, and BusyParam the URI parameter that sets its busy wait.
// Built with -tags "cgosqlite sqlite_fts5", the program uses the C SQLite through cgo instead (see
// driver_cgo.go): only for comparing the two in ports/bench, releases stay pure Go.
const (
	Driver    = "sqlite"
	BusyParam = "_pragma=busy_timeout(30000)"
)
