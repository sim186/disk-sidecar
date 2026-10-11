# Sidecar

This folder is the catalog of the disk it sits on. It is created and updated by Disk Sidecar
(https://github.com/sim186/disk-sidecar), version in `VERSION`.

- **Open the catalog:** double click `open-sidecar.command` (Mac) or `open-sidecar.bat` (Windows).
  Nothing else to install. The program for each system is in `bin/`; `sidecar` (or `sidecar.cmd`
  on Windows) runs the right one: `./sidecar serve | scan | hash | merge | mcp | version`.
- **index.db** is a SQLite file describing every file on this disk (and only this disk). Anyone
  or any program that can run SQL can read it; open it read-only.
- Your marks, the quarantine and the history of changes are stored in the same file.
- Do not edit or delete files in this folder by hand, except `index.db.bak` and `log.csv`
  if you want to.

## Reading the index

```sql
-- by name
SELECT path, size FROM files WHERE name LIKE '%invoice%';
-- inside text, code and PDFs
SELECT f.path FROM file_text t JOIN files f ON f.id = t.rowid WHERE file_text MATCH '"tax"*';
-- biggest folders
SELECT top, COUNT(*), SUM(size) FROM files GROUP BY top ORDER BY 3 DESC;
-- identical files
SELECT sha256, COUNT(*) c, size*(COUNT(*)-1) wasted FROM files
  WHERE sha256 IS NOT NULL GROUP BY sha256 HAVING c > 1 ORDER BY wasted DESC;
```

Tables: `sources(id, label, root, host, scanned_at)`, `files(id, source_id, path, name, ext, size,
mtime, kind, top, sha256)` with `path` relative to the disk root and `/` separated, `file_text`
(FTS5, `rowid` = `files.id`), `marks`, `meta` (`schema_version`, `disk_id`, `sidecar_version`),
`scan_log`, `quarantine_moves`, `quarantine_files`. `kind` is one of photo, video, audio,
document, code, installer, archive, lightroom, other.

For an LLM: `./sidecar mcp` exposes this index as MCP tools
(`claude mcp add disk-sidecar -- /path/to/this/folder/sidecar mcp`).
