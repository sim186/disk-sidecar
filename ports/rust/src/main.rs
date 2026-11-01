//! sidecar-scan-rs: a Rust port of `sidecar scan` (internal/scan + internal/idx of the Go program).
//!
//! It walks a folder read-only and records every file in the same SQLite index, with the same
//! layout, the same rows, in the same order, and the same incremental rules: files whose size and
//! date did not change are skipped, files gone from the disk are forgotten. See ports/README.md.
//!
//! Paths are kept as bytes end to end (a Unix file name need not be UTF-8), like Go's strings.

use rusqlite::types::{ToSqlOutput, ValueRef};
use rusqlite::{params, Connection, OptionalExtension, ToSql};
use sha2::{Digest, Sha256};
use std::collections::{HashMap, HashSet};
use std::ffi::OsStr;
use std::fs;
use std::io::{self, Read, Write};
use std::os::unix::ffi::OsStrExt;
use std::os::unix::fs::MetadataExt;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

const VERSION: &str = include_str!("../../../internal/assets/VERSION");
const SCHEMA_VERSION: i64 = 1;
const MAX_TEXT_BYTES: usize = 256 * 1024;

const DDL: &str = "
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT);
CREATE TABLE IF NOT EXISTS sources (
    id INTEGER PRIMARY KEY, label TEXT UNIQUE NOT NULL, root TEXT, host TEXT, scanned_at INTEGER);
CREATE TABLE IF NOT EXISTS files (
    id INTEGER PRIMARY KEY,
    source_id INTEGER NOT NULL REFERENCES sources(id),
    path TEXT NOT NULL,
    name TEXT NOT NULL,
    ext TEXT,
    size INTEGER NOT NULL,
    mtime INTEGER NOT NULL,
    kind TEXT NOT NULL,
    top TEXT,
    sha256 TEXT,
    seen INTEGER NOT NULL DEFAULT 0,
    UNIQUE (source_id, path));
CREATE INDEX IF NOT EXISTS files_size ON files(size);
CREATE INDEX IF NOT EXISTS files_hash ON files(sha256);
CREATE INDEX IF NOT EXISTS files_kind ON files(kind);
CREATE INDEX IF NOT EXISTS files_top ON files(source_id, top);
CREATE TABLE IF NOT EXISTS marks (
    source_id INTEGER NOT NULL, path TEXT NOT NULL,
    mark TEXT NOT NULL CHECK (mark IN ('keep','delete','review')), note TEXT,
    PRIMARY KEY (source_id, path));
CREATE VIRTUAL TABLE IF NOT EXISTS file_text USING fts5(
    path, body, tokenize='unicode61 remove_diacritics 2');
CREATE TABLE IF NOT EXISTS quarantine_moves (
    id INTEGER PRIMARY KEY, batch TEXT, source_id INTEGER, path TEXT, qpath TEXT,
    is_dir INTEGER, nfiles INTEGER, size INTEGER, moved_at INTEGER, state TEXT DEFAULT 'moved');
CREATE TABLE IF NOT EXISTS quarantine_files (
    move_id INTEGER, path TEXT, name TEXT, ext TEXT, size INTEGER, mtime INTEGER,
    kind TEXT, top TEXT, sha256 TEXT);
CREATE INDEX IF NOT EXISTS quarantine_files_move ON quarantine_files(move_id);
CREATE TABLE IF NOT EXISTS scan_log (
    time INTEGER, source_id INTEGER, kind TEXT, new INTEGER, changed INTEGER,
    removed INTEGER, errors INTEGER, secs REAL);
";

const UPSERT_FILE: &str = "INSERT INTO files(source_id,path,name,ext,size,mtime,kind,top,sha256,seen) VALUES (?,?,?,?,?,?,?,?,?,1)
	ON CONFLICT(source_id,path) DO UPDATE SET size=excluded.size, mtime=excluded.mtime, kind=excluded.kind,
	top=excluded.top, sha256=excluded.sha256, seen=1 RETURNING id";

// ------------------------------------------------------------------ the kinds of files

fn kind_of(ext: &[u8]) -> &'static str {
    // a match on byte strings compiles to a decision tree: no hashing, no allocation
    match ext {
        b"jpg" | b"jpeg" | b"png" | b"heic" | b"gif" | b"tif" | b"tiff" | b"dng" | b"nef" | b"arw" | b"cr2"
        | b"cr3" | b"raf" | b"orf" | b"rw2" | b"webp" | b"bmp" | b"psd" | b"xmp" => "photo",
        b"mkv" | b"mp4" | b"mov" | b"avi" | b"m4v" | b"wmv" | b"mpg" | b"mpeg" | b"mts" | b"m2ts" | b"flv"
        | b"webm" => "video",
        b"mp3" | b"flac" | b"wav" | b"m4a" | b"aac" | b"ogg" | b"aiff" => "audio",
        b"pdf" | b"doc" | b"docx" | b"xls" | b"xlsx" | b"ppt" | b"pptx" | b"odt" | b"ods" | b"txt" | b"md"
        | b"rtf" | b"csv" | b"epub" | b"pages" | b"numbers" | b"key" => "document",
        b"py" | b"js" | b"ts" | b"tsx" | b"jsx" | b"c" | b"h" | b"cpp" | b"hpp" | b"cs" | b"java" | b"kt"
        | b"swift" | b"go" | b"rs" | b"ino" | b"html" | b"css" | b"json" | b"yml" | b"yaml" | b"xml" | b"sh"
        | b"sql" | b"php" | b"rb" | b"class" | b"pyc" => "code",
        b"exe" | b"msi" | b"dmg" | b"pkg" | b"iso" | b"deb" | b"apk" | b"inf" | b"dll" | b"sys" => "installer",
        b"zip" | b"rar" | b"7z" | b"tar" | b"gz" | b"tgz" | b"bz2" | b"xz" => "archive",
        b"lrcat" | b"lrdata" | b"lrprev" | b"lrfprev" | b"lrtemplate" => "lightroom",
        _ => "other",
    }
}

fn is_text_ext(ext: &[u8]) -> bool {
    matches!(
        ext,
        b"txt" | b"md" | b"csv" | b"py" | b"js" | b"ts" | b"tsx" | b"jsx" | b"c" | b"h" | b"cpp" | b"hpp" | b"cs"
            | b"java" | b"kt" | b"swift" | b"go" | b"rs" | b"ino" | b"html" | b"css" | b"json" | b"yml" | b"yaml"
            | b"xml" | b"sh" | b"sql" | b"php" | b"rb"
    )
}

const SKIP_DIRS: &[&[u8]] = &[
    b"$RECYCLE.BIN", b"System Volume Information", b".Spotlight-V100", b".Trashes", b".fseventsd",
    b".TemporaryItems", b".sidecar",
];
const SKIP_FILES: &[&[u8]] =
    &[b".DS_Store", b"Thumbs.db", b"desktop.ini", b"Open Sidecar.command", b"Open Sidecar.bat"];
const NO_TEXT_DIRS: &[&str] = &[
    "node_modules", ".git", ".out", "build", "vcpkg_installed", "target", "venv", ".venv", "__pycache__", "Pods",
    "site-packages", ".gradle",
];
const HOME_SKIP_DIRS: &[&[u8]] =
    &[b"node_modules", b"__pycache__", b"venv", b"site-packages", b"Pods", b"vcpkg_installed"];

fn skip_file(name: &[u8]) -> bool {
    SKIP_FILES.contains(&name) || name.starts_with(b"._")
}

fn no_text_key() -> String {
    let mut ds: Vec<&str> = NO_TEXT_DIRS.to_vec();
    ds.sort_unstable(); // byte order, like Go's sort.Strings
    ds.join("/")
}

fn text_allowed(rel: &[u8]) -> bool {
    let mut parts = rel.split(|&b| b == b'/').peekable();
    while let Some(p) = parts.next() {
        if parts.peek().is_none() {
            break; // the file name itself
        }
        if NO_TEXT_DIRS.iter().any(|d| d.as_bytes() == p) {
            return false;
        }
    }
    true
}

/// The file name, the lower-case extension (".bashrc" has none) and the top folder of a path.
fn name_parts(rel: &[u8]) -> (&[u8], Vec<u8>, &[u8]) {
    let name = match rel.iter().rposition(|&b| b == b'/') {
        Some(i) => &rel[i + 1..],
        None => rel,
    };
    let ext = match name.iter().rposition(|&b| b == b'.') {
        Some(i) if i > 0 => to_lower(&name[i + 1..]),
        _ => Vec::new(),
    };
    let top = match rel.iter().position(|&b| b == b'/') {
        Some(i) => &rel[..i],
        None => b"",
    };
    (name, ext, top)
}

/// Lower case like Go's strings.ToLower: ASCII fast path; otherwise per rune, invalid bytes become U+FFFD.
fn to_lower(s: &[u8]) -> Vec<u8> {
    if s.is_ascii() {
        return s.to_ascii_lowercase();
    }
    let mut out = String::with_capacity(s.len());
    for chunk in s.utf8_chunks() {
        for c in chunk.valid().chars() {
            let mut l = c.to_lowercase();
            match (l.next(), l.next()) {
                (Some(one), None) => out.push(one),
                _ => out.push(c), // Go maps one rune to one rune; keep it rather than expand
            }
        }
        for _ in chunk.invalid() {
            out.push('\u{FFFD}');
        }
    }
    out.into_bytes()
}

/// Go's strings.ToValidUTF8(s, "�"): each run of invalid bytes becomes one U+FFFD.
fn to_valid_utf8(b: Vec<u8>) -> Vec<u8> {
    if std::str::from_utf8(&b).is_ok() {
        return b;
    }
    let mut out = Vec::with_capacity(b.len() + 16);
    let mut in_bad_run = false;
    for chunk in b.utf8_chunks() {
        if !chunk.valid().is_empty() {
            out.extend_from_slice(chunk.valid().as_bytes());
            in_bad_run = false;
        }
        if !chunk.invalid().is_empty() && !in_bad_run {
            out.extend_from_slice("\u{FFFD}".as_bytes());
            in_bad_run = true;
        }
    }
    out
}

/// Bytes bound as SQLite TEXT, whatever their encoding (Go binds its strings the same way).
struct Text<'a>(&'a [u8]);
impl ToSql for Text<'_> {
    fn to_sql(&self) -> rusqlite::Result<ToSqlOutput<'_>> {
        Ok(ToSqlOutput::Borrowed(ValueRef::Text(self.0)))
    }
}

// ------------------------------------------------------------------ reading files

fn sha256_file(path: &Path) -> io::Result<String> {
    let mut f = fs::File::open(path)?;
    let mut h = Sha256::new();
    let mut buf = vec![0u8; 4 << 20];
    loop {
        let n = match f.read(&mut buf) {
            Ok(0) => break,
            Ok(n) => n,
            Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
            Err(e) => return Err(e),
        };
        h.update(&buf[..n]);
    }
    let sum = h.finalize();
    let mut hex = String::with_capacity(64);
    for b in sum.iter() {
        use std::fmt::Write as _;
        let _ = write!(hex, "{b:02x}");
    }
    Ok(hex)
}

/// The searchable text of a file, or empty.
fn read_text(path: &Path, ext: &[u8]) -> Vec<u8> {
    if is_text_ext(ext) {
        let Ok(f) = fs::File::open(path) else { return Vec::new() };
        let mut b = Vec::new();
        let _ = f.take(MAX_TEXT_BYTES as u64).read_to_end(&mut b);
        return to_valid_utf8(b);
    }
    if ext == b"pdf" {
        return pdf_text(path);
    }
    Vec::new()
}

/// pdftotext, killed after 30 seconds like the Go program's context.
fn pdf_text(path: &Path) -> Vec<u8> {
    let Ok(mut child) = Command::new("pdftotext")
        .args([OsStr::new("-q"), OsStr::new("-l"), OsStr::new("30"), path.as_os_str(), OsStr::new("-")])
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .spawn()
    else {
        return Vec::new();
    };
    let mut out;
    let mut stdout = child.stdout.take().unwrap();
    let deadline = Instant::now() + Duration::from_secs(30);
    let reader = std::thread::spawn(move || {
        let mut b = Vec::new();
        let _ = stdout.read_to_end(&mut b);
        b
    });
    loop {
        match child.try_wait() {
            Ok(Some(status)) => {
                let b = reader.join().unwrap_or_default();
                if !status.success() {
                    return Vec::new();
                }
                out = b;
                break;
            }
            Ok(None) if Instant::now() < deadline => std::thread::sleep(Duration::from_millis(5)),
            _ => {
                let _ = child.kill();
                let _ = child.wait();
                return Vec::new();
            }
        }
    }
    out.truncate(MAX_TEXT_BYTES);
    to_valid_utf8(out)
}

// ------------------------------------------------------------------ the operating system

/// Background band for this thread: the system throttles its disk reads and gives it the CPU last.
fn background() {
    #[cfg(target_os = "macos")]
    unsafe {
        const PRIO_DARWIN_THREAD: libc::c_int = 3;
        const PRIO_DARWIN_BG: libc::c_int = 0x1000;
        libc::setpriority(PRIO_DARWIN_THREAD, 0, PRIO_DARWIN_BG);
    }
    #[cfg(target_os = "linux")]
    unsafe {
        let tid = libc::syscall(libc::SYS_gettid) as libc::id_t;
        libc::setpriority(libc::PRIO_PROCESS, tid, 10);
        libc::syscall(libc::SYS_ioprio_set, 1, tid, (2 << 13) | 7);
    }
}

/// Contents only in the cloud (iCloud "Optimise Mac Storage"): reading would download them.
#[allow(unused_variables)]
fn placeholder(md: &fs::Metadata) -> bool {
    #[cfg(target_os = "macos")]
    {
        use std::os::macos::fs::MetadataExt as _;
        const SF_DATALESS: u32 = 0x4000_0000;
        return md.st_flags() & SF_DATALESS != 0;
    }
    #[allow(unreachable_code)]
    false
}

/// Go's filepath.Abs: join with the working directory and clean lexically (no link resolution).
fn abs_clean(p: &Path) -> PathBuf {
    let joined = if p.is_absolute() { p.to_path_buf() } else { std::env::current_dir().unwrap().join(p) };
    let mut out: Vec<&OsStr> = Vec::new();
    for c in joined.components() {
        use std::path::Component::*;
        match c {
            RootDir | Prefix(_) => out.clear(),
            CurDir => {}
            ParentDir => {
                out.pop();
            }
            Normal(s) => out.push(s),
        }
    }
    let mut r = PathBuf::from("/");
    for s in out {
        r.push(s);
    }
    r
}

fn is_home(root: &Path) -> bool {
    let Some(h) = std::env::var_os("HOME").filter(|h| !h.is_empty()) else { return false };
    let a = fs::canonicalize(root).unwrap_or_else(|_| root.to_path_buf());
    let b = fs::canonicalize(&h).unwrap_or_else(|_| PathBuf::from(&h));
    abs_clean(&a) == abs_clean(&b)
}

fn hostname() -> String {
    let mut buf = [0u8; 256];
    let r = unsafe { libc::gethostname(buf.as_mut_ptr() as *mut libc::c_char, buf.len()) };
    if r != 0 {
        return String::new();
    }
    let n = buf.iter().position(|&b| b == 0).unwrap_or(buf.len());
    String::from_utf8_lossy(&buf[..n]).into_owned()
}

fn now_unix() -> i64 {
    SystemTime::now().duration_since(UNIX_EPOCH).map(|d| d.as_secs() as i64).unwrap_or(0)
}

// ------------------------------------------------------------------ the scan

struct Options {
    root: PathBuf,
    db: PathBuf,
    label: String,
    host: String,
    hash_min: i64,
    text: bool,
}

#[derive(Default)]
struct Stats {
    total: i64,
    new: i64,
    changed: i64,
    same: i64,
    removed: i64,
    errors: i64,
    size: i64,
    secs: f64,
}

type Error = Box<dyn std::error::Error>;

struct Scanner<'c> {
    o: &'c Options,
    conn: &'c Connection,
    sid: i64,
    root_len: usize,
    known: HashMap<Vec<u8>, (i64, i64)>,
    seen: HashSet<Vec<u8>>,
    st: Stats,
    count: u64,
    dirty_since: Option<Instant>,
    t0: Instant,
    home_mode: bool,
}

impl Scanner<'_> {
    /// Records one file; local is where it can be read (None = only in the cloud).
    fn process(&mut self, rel: Vec<u8>, size: i64, mtime: i64, local: Option<&Path>) -> Result<(), Error> {
        self.count += 1;
        // commit every 5000 files, and once a write has waited for 200 ms (see the Go program)
        if self.count % 5000 == 0 || self.dirty_since.is_some_and(|t| t.elapsed() > Duration::from_millis(200)) {
            self.dirty_since = None;
            self.conn.execute_batch("COMMIT; BEGIN")?;
            eprintln!("  {} files ({:.0}s)", self.count, self.t0.elapsed().as_secs_f64());
        }
        let prev = self.known.get(&rel).copied();
        if prev == Some((size, mtime)) {
            self.st.same += 1;
            self.seen.insert(rel);
            return Ok(());
        }
        let (name, ext, top) = name_parts(&rel);
        let mut sum: Option<String> = None;
        if let Some(local) = local {
            if self.o.hash_min >= 0 && size >= self.o.hash_min {
                match sha256_file(local) {
                    Ok(h) => sum = Some(h),
                    Err(e) => {
                        self.st.errors += 1;
                        eprintln!("hash error {}: {e}", String::from_utf8_lossy(&rel));
                    }
                }
            }
        }
        if self.dirty_since.is_none() {
            self.dirty_since = Some(Instant::now());
        }
        let id: i64 = {
            let mut stmt = self.conn.prepare_cached(UPSERT_FILE)?;
            stmt.query_row(
                params![self.sid, Text(&rel), Text(name), Text(&ext), size, mtime, kind_of(&ext), Text(top), sum],
                |r| r.get(0),
            )?
        };
        if prev.is_some() {
            self.st.changed += 1;
        } else {
            self.st.new += 1;
        }
        if let Some(local) = local {
            if self.o.text && text_allowed(&rel) {
                let body = read_text(local, &ext);
                if !body.is_empty() {
                    self.conn.prepare_cached("DELETE FROM file_text WHERE rowid=?")?.execute([id])?;
                    self.conn
                        .prepare_cached("INSERT INTO file_text(rowid, path, body) VALUES (?,?,?)")?
                        .execute(params![id, Text(&rel), Text(&body)])?;
                }
            }
        }
        self.seen.insert(rel);
        Ok(())
    }

    /// Depth first, entries in byte order: the order of Go's filepath.WalkDir, so rows get the same ids.
    fn walk(&mut self, dir: &Path) -> Result<(), Error> {
        let rd = match fs::read_dir(dir) {
            Ok(rd) => rd,
            Err(e) => {
                self.st.errors += 1;
                eprintln!("read error {}: {e}", dir.display());
                return Ok(());
            }
        };
        let mut entries: Vec<(Vec<u8>, fs::FileType)> = Vec::new();
        for e in rd {
            match e {
                Ok(e) => match e.file_type() {
                    Ok(t) => entries.push((e.file_name().as_bytes().to_vec(), t)),
                    Err(_) => continue,
                },
                Err(e) => {
                    self.st.errors += 1;
                    eprintln!("read error {}: {e}", dir.display());
                    break;
                }
            }
        }
        entries.sort_unstable_by(|a, b| a.0.cmp(&b.0));
        for (name, ft) in entries {
            let path = dir.join(OsStr::from_bytes(&name));
            if ft.is_dir() {
                if SKIP_DIRS.contains(&name.as_slice()) {
                    continue;
                }
                if self.home_mode
                    && (name.starts_with(b".")
                        || HOME_SKIP_DIRS.contains(&name.as_slice())
                        || (dir == self.o.root && (name == b"Library" || name == b"AppData")))
                {
                    continue;
                }
                self.walk(&path)?;
                continue;
            }
            if skip_file(&name) {
                continue;
            }
            // like a normal stat: follow links to files, ignore links to folders
            let md = if ft.is_symlink() { fs::metadata(&path) } else { fs::symlink_metadata(&path) };
            let md = match md {
                Ok(m) => m,
                Err(e) => {
                    self.st.errors += 1;
                    eprintln!("stat error {}: {e}", path.display());
                    continue;
                }
            };
            if !md.file_type().is_file() {
                continue;
            }
            let rel = path.as_os_str().as_bytes()[self.root_len..].to_vec();
            let local = if placeholder(&md) { None } else { Some(path.as_path()) };
            self.process(rel, md.len() as i64, md.mtime(), local)?;
        }
        Ok(())
    }
}

fn open_index(path: &Path) -> Result<Connection, Error> {
    let conn = Connection::open(path).map_err(|e| format!("{}: {e}", path.display()))?;
    conn.busy_timeout(Duration::from_secs(30))?;
    // rusqlite's bundled SQLite turns foreign keys on by default; SQLite itself (and the Go program) does not
    conn.execute_batch("PRAGMA foreign_keys=OFF")?;
    conn.execute_batch(DDL).map_err(|e| format!("{}: {e}", path.display()))?;
    let v: Option<String> =
        conn.query_row("SELECT value FROM meta WHERE key='schema_version'", [], |r| r.get(0)).optional()?;
    if let Some(v) = v {
        if v.parse::<i64>().unwrap_or(0) > SCHEMA_VERSION {
            return Err(format!("this index uses schema {v}, newer than this program supports ({SCHEMA_VERSION})").into());
        }
    }
    Ok(conn)
}

fn run(o: &Options) -> Result<Stats, Error> {
    let t0 = Instant::now();
    match fs::metadata(&o.root) {
        Ok(m) if m.is_dir() => {}
        _ => return Err(format!("not a folder: {}", o.root.display()).into()),
    }
    let conn = open_index(&o.db)?;
    let mut disk_id = [0u8; 16];
    getrandom::fill(&mut disk_id).map_err(|e| e.to_string())?;
    let disk_id: String = disk_id.iter().map(|b| format!("{b:02x}")).collect();
    conn.execute("INSERT OR REPLACE INTO meta VALUES ('schema_version', ?)", [SCHEMA_VERSION.to_string()])?;
    conn.execute("INSERT OR REPLACE INTO meta VALUES ('sidecar_version', ?)", [VERSION.trim()])?;
    conn.execute("INSERT OR IGNORE INTO meta VALUES ('disk_id', ?)", [disk_id])?;
    let root_bytes = o.root.as_os_str().as_bytes();
    conn.execute(
        "INSERT INTO sources(label, root, host) VALUES (?,?,?)
		ON CONFLICT(label) DO UPDATE SET root=excluded.root, host=excluded.host",
        params![o.label, Text(root_bytes), o.host],
    )?;
    let sid: i64 = conn.query_row("SELECT id FROM sources WHERE label=?", [&o.label], |r| r.get(0))?;

    let mut known = HashMap::new();
    {
        let mut q = conn.prepare("SELECT path, size, mtime FROM files WHERE source_id=?")?;
        let mut rows = q.query([sid])?;
        while let Some(r) = rows.next()? {
            let p = match r.get_ref(0)? {
                ValueRef::Text(b) | ValueRef::Blob(b) => b.to_vec(),
                _ => continue,
            };
            known.insert(p, (r.get::<_, i64>(1)?, r.get::<_, i64>(2)?));
        }
    }
    let seen = HashSet::with_capacity(known.len());
    let root_len = if root_bytes.ends_with(b"/") { root_bytes.len() } else { root_bytes.len() + 1 };

    let mut s = Scanner {
        o,
        conn: &conn,
        sid,
        root_len,
        known,
        seen,
        st: Stats::default(),
        count: 0,
        dirty_since: None,
        t0,
        home_mode: is_home(&o.root),
    };
    conn.execute_batch("BEGIN")?;
    if let Err(e) = s.walk(&o.root) {
        let _ = conn.execute_batch("ROLLBACK");
        return Err(e);
    }
    let finish = || -> Result<(), Error> {
        {
            let mut q1 = conn.prepare_cached(
                "DELETE FROM file_text WHERE rowid IN (SELECT id FROM files WHERE source_id=? AND path=?)",
            )?;
            let mut q2 = conn.prepare_cached("DELETE FROM files WHERE source_id=? AND path=?")?;
            for p in s.known.keys() {
                if s.seen.contains(p) {
                    continue;
                }
                q1.execute(params![sid, Text(p)])?;
                q2.execute(params![sid, Text(p)])?;
            }
        }
        let cleaned = format!("notext_dirs:{sid}");
        let done: Option<String> =
            conn.query_row("SELECT value FROM meta WHERE key=?", [&cleaned], |r| r.get(0)).optional()?;
        let key = no_text_key();
        if o.text && done.as_deref() != Some(key.as_str()) {
            for d in NO_TEXT_DIRS {
                let like = like_prefix(d);
                conn.execute(
                    "DELETE FROM file_text WHERE rowid IN (SELECT id FROM files WHERE source_id=? AND (path LIKE ? ESCAPE '\\' OR path LIKE ? ESCAPE '\\'))",
                    params![sid, like, format!("%/{like}")],
                )?;
            }
            conn.execute("INSERT OR REPLACE INTO meta VALUES (?, ?)", params![cleaned, key])?;
        }
        conn.execute("UPDATE sources SET scanned_at=? WHERE id=?", params![now_unix(), sid])?;
        Ok(())
    };
    if let Err(e) = finish() {
        let _ = conn.execute_batch("ROLLBACK");
        return Err(e);
    }
    s.st.removed = s.known.keys().filter(|p| !s.seen.contains(*p)).count() as i64;
    conn.execute_batch("COMMIT")?;
    let mut st = s.st;
    (st.total, st.size) = conn.query_row(
        "SELECT COUNT(*), COALESCE(SUM(size),0) FROM files WHERE source_id=?",
        [sid],
        |r| Ok((r.get(0)?, r.get(1)?)),
    )?;
    st.secs = t0.elapsed().as_secs_f64();
    Ok(st)
}

fn like_prefix(rel: &str) -> String {
    rel.replace('\\', "\\\\").replace('%', "\\%").replace('_', "\\_") + "/%"
}

// ------------------------------------------------------------------ the command line

fn die(msg: impl std::fmt::Display) -> ! {
    eprintln!("sidecar: {msg}");
    std::process::exit(1)
}

fn main() {
    let mut args = std::env::args_os().skip(1);
    let (mut db, mut source, mut hash_mb, mut no_text, mut with_text) = (None, None, -1.0f64, false, false);
    let mut pos: Vec<PathBuf> = Vec::new();
    while let Some(a) = args.next() {
        let s = a.to_string_lossy().into_owned();
        let name = s.trim_start_matches('-');
        let (name, inline) = match name.split_once('=') {
            Some((n, v)) => (n.to_string(), Some(v.to_string())),
            None => (name.to_string(), None),
        };
        if !s.starts_with('-') || s == "-" {
            pos.push(PathBuf::from(a));
            continue;
        }
        let mut value = || inline.clone().or_else(|| args.next().map(|v| v.to_string_lossy().into_owned()));
        match name.as_str() {
            "db" => db = value().map(PathBuf::from),
            "source" => source = value(),
            "hash-min-mb" => hash_mb = value().and_then(|v| v.parse().ok()).unwrap_or_else(|| die("bad --hash-min-mb")),
            "no-text" => no_text = true,
            "text" => with_text = true,
            "version" => {
                println!("sidecar-scan-rs {}", VERSION.trim());
                return;
            }
            _ => die(format!("unknown flag {s}\nusage: sidecar-scan-rs <folder> [--db index.db] [--source name] [--hash-min-mb N] [--no-text] [--text]")),
        }
    }
    if pos.len() != 1 {
        die("usage: sidecar-scan-rs <folder> [--db index.db] [--source name]");
    }
    let root = abs_clean(&pos[0]);
    let db = db.unwrap_or_else(|| root.join(".sidecar").join("index.db"));
    let label = source.unwrap_or_else(|| {
        root.file_name().map(|n| n.to_string_lossy().into_owned()).unwrap_or_else(|| "/".into())
    });
    let hash_min = if hash_mb >= 0.0 { (hash_mb * 1024.0 * 1024.0) as i64 } else { -1 };
    let o = Options { text: !no_text && (with_text || !is_home(&root)), root, db, label, host: hostname(), hash_min };
    if std::env::var_os("SIDECAR_SCAN_FOREGROUND").is_none() {
        background(); // see the Go program: SIDECAR_SCAN_FOREGROUND is set only by ports/bench
    }
    match run(&o) {
        Ok(st) => {
            println!(
                "[{}] {} files, {:.1} GB | new {}, changed {}, same {}, removed {}, errors {} | {:.0}s",
                o.label,
                st.total,
                st.size as f64 / 1e9,
                st.new,
                st.changed,
                st.same,
                st.removed,
                st.errors,
                st.secs
            );
            let _ = io::stdout().flush();
        }
        Err(e) => die(e),
    }
}
