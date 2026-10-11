# Changelog

All notable changes to Disk Sidecar. Versions follow [Semantic Versioning](https://semver.org/);
the layout of the index has its own number (`schema_version` in the index's `meta` table),
which only changes when older programs could no longer read or write it safely.

## 0.1.0

First public version.

- One sidecar per disk: `<disk>/.sidecar/index.db` describes only that disk. Plugged-in disks
  are found and shown together; snapshots of other computers can be added read-only.
- Scanner (incremental), duplicate confirmation by SHA-256 (also across disks), merge of another
  index, MCP server for LLMs.
- Browser app: map of folders, search in names and contents, duplicates, folder comparison,
  marks (keep / delete / review) stored in the disk's own index.
- Quarantine: files marked for deletion are moved, not deleted, and can be restored; emptying it
  needs a typed confirmation. The server refuses to remove the last copy of a duplicated file.
- Re-check of every plugged-in disk at start, reporting what changed by hand.
- `install.py` installs or updates the sidecar on a disk without touching its data.
