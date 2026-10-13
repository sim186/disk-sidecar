# Disk Sidecar

Go program (`cmd/sidecar`, `internal/...`) that serves a single-page app (`internal/assets/web/catalog.html`,
sql.js in the browser) over a SQLite index that lives on each external disk.

## Checks

- `go vet ./... && go test ./...` before calling a change done.
- **Performance harness**: run it after any change to `catalog.html` rendering or SQL, `internal/server`
  (marks, index copy, verify), `internal/scan` or `internal/idx`, and report the table it prints:

  ```sh
  SIDECAR_PERF=1 go test ./tests -run TestPerf -v
  ```

  It fails when a measure goes over its budget (`perfBudgets` in `tests/perf_test.go`). Do not raise a
  budget to make a change pass without saying so and why. The page measures need Node 22+ and Chrome; if
  they print "skipped", say so rather than reporting a pass.

## Performance rules the harness enforces

- A click on Keep / Not sure / Let go runs **no SQL** in the page: `q()` memoizes reads per loaded copy
  (`qcache`, reset in `open()`), and the view is redrawn from memory. New read queries go through `q()`;
  never mutate the rows it returns.
- Folder filters use `where(pre)`: a range on the `(source_id, path)` index, never `substr(path, …) = ?`,
  so a folder costs what it holds, not the whole disk.
- Writers to a disk's index inside the server take turns through `Server.dbw` (marks) and
  `scan.Options.WriteLock` (verify): SQLite's busy wait alone starves a click while a check runs.
- Stop a server you started with Ctrl+C / `kill -INT`, never `kill -9`: only a clean stop removes its copy of
  the catalog from the temporary folder. Tests use `stopServer`. A server stops by itself after `--idle-exit`
  (30 min) without requests; lifecycle rules are in `internal/server/life.go`, tested by `tests/life_test.go`.
- The disks can be slow USB hard disks: keep write transactions short and avoid full-table passes on
  every verify.
