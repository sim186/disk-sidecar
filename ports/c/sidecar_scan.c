/*
 * sidecar-scan-c: a C port of `sidecar scan` (internal/scan + internal/idx of the Go program).
 *
 * It walks a folder read-only and records every file in the same SQLite index, with the same
 * layout, the same rows, in the same order, and the same incremental rules: files whose size and
 * date did not change are skipped, files gone from the disk are forgotten. See ports/README.md.
 *
 * C11 + POSIX. Everything the Go and Rust standard libraries give for free is written here: a hash
 * table, a growable buffer, UTF-8 validation, SHA-256 (or CommonCrypto on macOS), the directory walk.
 */
#define _GNU_SOURCE
#define _DARWIN_C_SOURCE
#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <poll.h>
#include <signal.h>
#include <spawn.h>
#include <stdarg.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/resource.h>
#include <sys/stat.h>
#include <sys/time.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>
#include <sys/random.h> /* getentropy */
#if defined(__linux__)
#include <sys/syscall.h>
#endif

#include "sqlite3.h"
#include "sha256.h"

#ifndef SIDECAR_VERSION
#define SIDECAR_VERSION "dev"
#endif

extern char **environ;

#define SCHEMA_VERSION 1
#define MAX_TEXT_BYTES (256 * 1024)

static const char *DDL =
    "CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT);\n"
    "CREATE TABLE IF NOT EXISTS sources (\n"
    "    id INTEGER PRIMARY KEY, label TEXT UNIQUE NOT NULL, root TEXT, host TEXT, scanned_at INTEGER);\n"
    "CREATE TABLE IF NOT EXISTS files (\n"
    "    id INTEGER PRIMARY KEY,\n"
    "    source_id INTEGER NOT NULL REFERENCES sources(id),\n"
    "    path TEXT NOT NULL,\n"
    "    name TEXT NOT NULL,\n"
    "    ext TEXT,\n"
    "    size INTEGER NOT NULL,\n"
    "    mtime INTEGER NOT NULL,\n"
    "    kind TEXT NOT NULL,\n"
    "    top TEXT,\n"
    "    sha256 TEXT,\n"
    "    seen INTEGER NOT NULL DEFAULT 0,\n"
    "    UNIQUE (source_id, path));\n"
    "CREATE INDEX IF NOT EXISTS files_size ON files(size);\n"
    "CREATE INDEX IF NOT EXISTS files_hash ON files(sha256);\n"
    "CREATE INDEX IF NOT EXISTS files_kind ON files(kind);\n"
    "CREATE INDEX IF NOT EXISTS files_top ON files(source_id, top);\n"
    "CREATE TABLE IF NOT EXISTS marks (\n"
    "    source_id INTEGER NOT NULL, path TEXT NOT NULL,\n"
    "    mark TEXT NOT NULL CHECK (mark IN ('keep','delete','review')), note TEXT,\n"
    "    PRIMARY KEY (source_id, path));\n"
    "CREATE VIRTUAL TABLE IF NOT EXISTS file_text USING fts5(\n"
    "    path, body, tokenize='unicode61 remove_diacritics 2');\n"
    "CREATE TABLE IF NOT EXISTS quarantine_moves (\n"
    "    id INTEGER PRIMARY KEY, batch TEXT, source_id INTEGER, path TEXT, qpath TEXT,\n"
    "    is_dir INTEGER, nfiles INTEGER, size INTEGER, moved_at INTEGER, state TEXT DEFAULT 'moved');\n"
    "CREATE TABLE IF NOT EXISTS quarantine_files (\n"
    "    move_id INTEGER, path TEXT, name TEXT, ext TEXT, size INTEGER, mtime INTEGER,\n"
    "    kind TEXT, top TEXT, sha256 TEXT);\n"
    "CREATE INDEX IF NOT EXISTS quarantine_files_move ON quarantine_files(move_id);\n"
    "CREATE TABLE IF NOT EXISTS scan_log (\n"
    "    time INTEGER, source_id INTEGER, kind TEXT, new INTEGER, changed INTEGER,\n"
    "    removed INTEGER, errors INTEGER, secs REAL);\n";

static const char *UPSERT_FILE =
    "INSERT INTO files(source_id,path,name,ext,size,mtime,kind,top,sha256,seen) VALUES (?,?,?,?,?,?,?,?,?,1)\n"
    "\tON CONFLICT(source_id,path) DO UPDATE SET size=excluded.size, mtime=excluded.mtime, kind=excluded.kind,\n"
    "\ttop=excluded.top, sha256=excluded.sha256, seen=1 RETURNING id";

/* ------------------------------------------------------------------ small helpers */

static sqlite3 *g_db;

static void die(const char *fmt, ...) {
    va_list ap;
    va_start(ap, fmt);
    fputs("sidecar: ", stderr);
    vfprintf(stderr, fmt, ap);
    fputc('\n', stderr);
    va_end(ap);
    if (g_db && !sqlite3_get_autocommit(g_db)) sqlite3_exec(g_db, "ROLLBACK", 0, 0, 0);
    if (g_db) sqlite3_close(g_db);
    exit(1);
}

static void *xmalloc(size_t n) {
    void *p = malloc(n ? n : 1);
    if (!p) die("out of memory");
    return p;
}

static void *xrealloc(void *p, size_t n) {
    p = realloc(p, n ? n : 1);
    if (!p) die("out of memory");
    return p;
}

static char *xstrndup(const char *s, size_t n) {
    char *d = xmalloc(n + 1);
    memcpy(d, s, n);
    d[n] = 0;
    return d;
}

static double now_secs(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return ts.tv_sec + ts.tv_nsec / 1e9;
}

/* growable byte buffer */
typedef struct {
    char *p;
    size_t n, cap;
} buf_t;

static void buf_put(buf_t *b, const void *s, size_t n) {
    if (b->n + n + 1 > b->cap) {
        b->cap = (b->n + n + 1) * 2;
        b->p = xrealloc(b->p, b->cap);
    }
    memcpy(b->p + b->n, s, n);
    b->n += n;
    b->p[b->n] = 0;
}

static void check(int rc, const char *what) {
    if (rc != SQLITE_OK && rc != SQLITE_DONE && rc != SQLITE_ROW) die("%s: %s", what, sqlite3_errmsg(g_db));
}

static void exec(const char *sql) {
    char *err = NULL;
    if (sqlite3_exec(g_db, sql, 0, 0, &err) != SQLITE_OK) die("%s", err ? err : sqlite3_errmsg(g_db));
}

static sqlite3_stmt *prepare(const char *sql) {
    sqlite3_stmt *st;
    check(sqlite3_prepare_v2(g_db, sql, -1, &st, NULL), sql);
    return st;
}

/* step a statement that returns no row, and reset it */
static void step_done(sqlite3_stmt *st) {
    int rc = sqlite3_step(st);
    if (rc != SQLITE_DONE && rc != SQLITE_ROW) die("%s", sqlite3_errmsg(g_db));
    sqlite3_reset(st);
    sqlite3_clear_bindings(st);
}

/* ------------------------------------------------------------------ hash table: path -> (size, mtime) */

enum { KNOWN = 1, SEEN = 2 };

typedef struct {
    char *key; /* NULL = empty slot */
    uint32_t len;
    uint32_t hash;
    int64_t size, mtime;
    uint8_t flags;
} entry_t;

typedef struct {
    entry_t *e;
    size_t cap, n;
} table_t;

static uint32_t fnv1a(const char *s, size_t n) {
    uint32_t h = 2166136261u;
    for (size_t i = 0; i < n; i++) h = (h ^ (uint8_t)s[i]) * 16777619u;
    return h;
}

static entry_t *table_find(table_t *t, const char *k, size_t n, uint32_t h) {
    size_t mask = t->cap - 1;
    for (size_t i = h & mask;; i = (i + 1) & mask) {
        entry_t *e = &t->e[i];
        if (!e->key || (e->hash == h && e->len == n && memcmp(e->key, k, n) == 0)) return e;
    }
}

static void table_grow(table_t *t) {
    table_t nt = {xmalloc(sizeof(entry_t) * t->cap * 2), t->cap * 2, t->n};
    memset(nt.e, 0, sizeof(entry_t) * nt.cap);
    for (size_t i = 0; i < t->cap; i++)
        if (t->e[i].key) *table_find(&nt, t->e[i].key, t->e[i].len, t->e[i].hash) = t->e[i];
    free(t->e);
    *t = nt;
}

/* returns the entry for k, inserting it (flags 0) if missing */
static entry_t *table_upsert(table_t *t, const char *k, size_t n) {
    if ((t->n + 1) * 4 > t->cap * 3) table_grow(t);
    uint32_t h = fnv1a(k, n);
    entry_t *e = table_find(t, k, n, h);
    if (!e->key) {
        e->key = xstrndup(k, n);
        e->len = (uint32_t)n;
        e->hash = h;
        t->n++;
    }
    return e;
}

/* ------------------------------------------------------------------ kinds, names, text */

static const char *KIND_GROUPS[][2] = {
    {"photo", "jpg jpeg png heic gif tif tiff dng nef arw cr2 cr3 raf orf rw2 webp bmp psd xmp"},
    {"video", "mkv mp4 mov avi m4v wmv mpg mpeg mts m2ts flv webm"},
    {"audio", "mp3 flac wav m4a aac ogg aiff"},
    {"document", "pdf doc docx xls xlsx ppt pptx odt ods txt md rtf csv epub pages numbers key"},
    {"code", "py js ts tsx jsx c h cpp hpp cs java kt swift go rs ino html css json yml yaml xml sh sql php rb class pyc"},
    {"installer", "exe msi dmg pkg iso deb apk inf dll sys"},
    {"archive", "zip rar 7z tar gz tgz bz2 xz"},
    {"lightroom", "lrcat lrdata lrprev lrfprev lrtemplate"},
};
static const char *TEXT_EXTS =
    "txt md csv py js ts tsx jsx c h cpp hpp cs java kt swift go rs ino html css json yml yaml xml sh sql php rb";

/* whether word (n bytes) is one of the space-separated words in list */
static bool in_words(const char *list, const char *w, size_t n) {
    if (n == 0) return false;
    for (const char *p = list; *p;) {
        const char *q = strchr(p, ' ');
        size_t l = q ? (size_t)(q - p) : strlen(p);
        if (l == n && memcmp(p, w, n) == 0) return true;
        if (!q) break;
        p = q + 1;
    }
    return false;
}

static const char *kind_of(const char *ext, size_t n) {
    for (size_t i = 0; i < sizeof KIND_GROUPS / sizeof KIND_GROUPS[0]; i++)
        if (in_words(KIND_GROUPS[i][1], ext, n)) return KIND_GROUPS[i][0];
    return "other";
}

static const char *SKIP_DIRS[] = {"$RECYCLE.BIN", "System Volume Information", ".Spotlight-V100", ".Trashes",
                                  ".fseventsd", ".TemporaryItems", ".sidecar", NULL};
static const char *SKIP_FILES[] = {".DS_Store", "Thumbs.db", "desktop.ini", "Open Sidecar.command",
                                   "Open Sidecar.bat", NULL};
/* sorted in byte order: the key that says which list of folders was cleaned is this list joined by "/" */
static const char *NO_TEXT_DIRS[] = {".git", ".gradle", ".out", ".venv", "Pods", "__pycache__", "build",
                                     "node_modules", "site-packages", "target", "vcpkg_installed", "venv", NULL};
static const char *HOME_SKIP_DIRS[] = {"node_modules", "__pycache__", "venv", "site-packages", "Pods",
                                       "vcpkg_installed", NULL};

static bool in_list(const char **l, const char *s, size_t n) {
    for (; *l; l++)
        if (strlen(*l) == n && memcmp(*l, s, n) == 0) return true;
    return false;
}

static bool skip_dir(const char *name) { return in_list(SKIP_DIRS, name, strlen(name)); }

/* memrchr is a GNU extension; macOS does not have it */
static const char *last_byte(const char *s, char c, size_t n) {
    while (n > 0)
        if (s[--n] == c) return s + n;
    return NULL;
}

static bool skip_file(const char *name) {
    return in_list(SKIP_FILES, name, strlen(name)) || (name[0] == '.' && name[1] == '_');
}

static bool text_allowed(const char *rel, size_t n) {
    const char *p = rel, *end = rel + n;
    for (;;) {
        const char *slash = memchr(p, '/', (size_t)(end - p));
        if (!slash) return true; /* the file name itself */
        if (in_list(NO_TEXT_DIRS, p, (size_t)(slash - p))) return false;
        p = slash + 1;
    }
}

/*
 * Length of the valid UTF-8 sequence at s (n bytes left), or 0 if invalid. The same rules as Go and
 * Rust: no overlong forms, no surrogates, nothing above U+10FFFF.
 */
static size_t utf8_seq(const unsigned char *s, size_t n) {
    unsigned char c = s[0];
    if (c < 0x80) return 1;
    size_t len;
    unsigned char lo = 0x80, hi = 0xBF;
    if (c >= 0xC2 && c <= 0xDF) len = 2;
    else if (c == 0xE0) len = 3, lo = 0xA0;
    else if (c >= 0xE1 && c <= 0xEC) len = 3;
    else if (c == 0xED) len = 3, hi = 0x9F;
    else if (c >= 0xEE && c <= 0xEF) len = 3;
    else if (c == 0xF0) len = 4, lo = 0x90;
    else if (c >= 0xF1 && c <= 0xF3) len = 4;
    else if (c == 0xF4) len = 4, hi = 0x8F;
    else return 0;
    if (n < len) return 0;
    if (s[1] < lo || s[1] > hi) return 0;
    for (size_t i = 2; i < len; i++)
        if (s[i] < 0x80 || s[i] > 0xBF) return 0;
    return len;
}

/* Go's strings.ToValidUTF8(s, "�"), in place into out: each run of invalid bytes becomes one U+FFFD */
static void to_valid_utf8(const unsigned char *s, size_t n, buf_t *out) {
    out->n = 0;
    bool bad = false;
    size_t i = 0, start = 0;
    while (i < n) {
        if (s[i] < 0x80) { i++; bad = false; continue; }
        size_t l = utf8_seq(s + i, n - i);
        if (l) { i += l; bad = false; continue; }
        if (i > start) buf_put(out, s + start, i - start);
        if (!bad) buf_put(out, "\xEF\xBF\xBD", 3);
        bad = true;
        start = ++i;
    }
    if (i > start) buf_put(out, s + start, i - start);
    if (!out->p) buf_put(out, "", 0);
}

/* ------------------------------------------------------------------ reading files */

static unsigned char *g_io; /* 4 MiB read buffer, shared by hashing and text */
#define IO_BUF (4u << 20)

static int sha256_file(const char *path, char hex[65]) {
    int fd = open(path, O_RDONLY | O_CLOEXEC);
    if (fd < 0) return -1;
    sha256_ctx c;
    sha256_init(&c);
    for (;;) {
        ssize_t r = read(fd, g_io, IO_BUF);
        if (r < 0 && errno == EINTR) continue;
        if (r < 0) { int e = errno; close(fd); errno = e; return -1; }
        if (r == 0) break;
        sha256_update(&c, g_io, (size_t)r);
    }
    close(fd);
    unsigned char sum[32];
    sha256_final(&c, sum);
    static const char *digits = "0123456789abcdef";
    for (int i = 0; i < 32; i++) hex[2 * i] = digits[sum[i] >> 4], hex[2 * i + 1] = digits[sum[i] & 15];
    hex[64] = 0;
    return 0;
}

/* pdftotext, killed after 30 seconds like the Go program's context; returns bytes read into g_io */
static size_t pdf_text(const char *path) {
    int fds[2];
    if (pipe(fds) != 0) return 0;
    posix_spawn_file_actions_t fa;
    posix_spawn_file_actions_init(&fa);
    posix_spawn_file_actions_adddup2(&fa, fds[1], 1);
    posix_spawn_file_actions_addclose(&fa, fds[0]);
    posix_spawn_file_actions_addopen(&fa, 2, "/dev/null", O_WRONLY, 0);
    char *argv[] = {"pdftotext", "-q", "-l", "30", (char *)path, "-", NULL};
    pid_t pid;
    int rc = posix_spawnp(&pid, "pdftotext", &fa, NULL, argv, environ);
    posix_spawn_file_actions_destroy(&fa);
    close(fds[1]);
    if (rc != 0) { close(fds[0]); return 0; }
    double deadline = now_secs() + 30;
    size_t n = 0;
    bool killed = false;
    for (;;) {
        int left = (int)((deadline - now_secs()) * 1000);
        if (left <= 0) { kill(pid, SIGKILL); killed = true; break; }
        struct pollfd p = {fds[0], POLLIN, 0};
        if (poll(&p, 1, left) <= 0) continue;
        unsigned char tmp[65536];
        ssize_t r = read(fds[0], tmp, sizeof tmp);
        if (r < 0 && errno == EINTR) continue;
        if (r <= 0) break;
        size_t take = (size_t)r;
        if (n + take > MAX_TEXT_BYTES) take = n < MAX_TEXT_BYTES ? MAX_TEXT_BYTES - n : 0;
        memcpy(g_io + n, tmp, take);
        n += take;
    }
    close(fds[0]);
    int status;
    while (waitpid(pid, &status, 0) < 0 && errno == EINTR) {}
    if (killed || !WIFEXITED(status) || WEXITSTATUS(status) != 0) return 0;
    return n;
}

/* the searchable text of a file into out (empty if none) */
static void read_text(const char *path, const char *ext, size_t extn, buf_t *out) {
    out->n = 0;
    size_t n = 0;
    if (in_words(TEXT_EXTS, ext, extn)) {
        int fd = open(path, O_RDONLY | O_CLOEXEC);
        if (fd < 0) return;
        while (n < MAX_TEXT_BYTES) {
            ssize_t r = read(fd, g_io + n, MAX_TEXT_BYTES - n);
            if (r < 0 && errno == EINTR) continue;
            if (r <= 0) break;
            n += (size_t)r;
        }
        close(fd);
    } else if (extn == 3 && memcmp(ext, "pdf", 3) == 0) {
        n = pdf_text(path);
    } else {
        return;
    }
    to_valid_utf8(g_io, n, out);
}

/* ------------------------------------------------------------------ the operating system */

static void background(void) {
#if defined(__APPLE__)
    const int prio_darwin_thread = 3, prio_darwin_bg = 0x1000;
    setpriority(prio_darwin_thread, 0, prio_darwin_bg);
#elif defined(__linux__)
    pid_t tid = (pid_t)syscall(SYS_gettid);
    setpriority(PRIO_PROCESS, (id_t)tid, 10);
    syscall(SYS_ioprio_set, 1, tid, (2 << 13) | 7);
#endif
}

static bool placeholder(const struct stat *st) {
#if defined(__APPLE__)
    return (st->st_flags & 0x40000000u) != 0; /* SF_DATALESS */
#else
    (void)st;
    return false;
#endif
}

/* Go's filepath.Abs: join with the working directory and clean lexically (no link resolution) */
static char *abs_clean(const char *p) {
    buf_t in = {0};
    if (p[0] != '/') {
        char cwd[4096];
        if (!getcwd(cwd, sizeof cwd)) die("getcwd: %s", strerror(errno));
        buf_put(&in, cwd, strlen(cwd));
        buf_put(&in, "/", 1);
    }
    buf_put(&in, p, strlen(p));
    char *out = xmalloc(in.n + 2);
    size_t o = 0;
    const char *s = in.p, *end = in.p + in.n;
    while (s < end) {
        while (s < end && *s == '/') s++;
        const char *e = s;
        while (e < end && *e != '/') e++;
        size_t l = (size_t)(e - s);
        if (l == 0 || (l == 1 && s[0] == '.')) {
        } else if (l == 2 && s[0] == '.' && s[1] == '.') {
            while (o > 0 && out[o - 1] != '/') o--;
            if (o > 0) o--;
        } else {
            out[o++] = '/';
            memcpy(out + o, s, l);
            o += l;
        }
        s = e;
    }
    if (o == 0) out[o++] = '/';
    out[o] = 0;
    free(in.p);
    return out;
}

static bool is_home(const char *root) {
    const char *h = getenv("HOME");
    if (!h || !*h) return false;
    char a[PATH_MAX], b[PATH_MAX];
    const char *ra = realpath(root, a) ? a : root, *rb = realpath(h, b) ? b : h;
    char *ca = abs_clean(ra), *cb = abs_clean(rb);
    bool eq = strcmp(ca, cb) == 0;
    free(ca);
    free(cb);
    return eq;
}

/* ------------------------------------------------------------------ the scan */

typedef struct {
    const char *root, *db, *label, *host;
    int64_t hash_min;
    bool text;
} options_t;

typedef struct {
    int64_t total, new_, changed, same, removed, errors, size;
    double secs;
} stats_t;

static options_t O;
static stats_t ST;
static table_t KNOWN_T;
static int64_t SID;
static size_t ROOT_LEN;
static bool HOME_MODE;
static uint64_t COUNT;
static double T0, DIRTY_SINCE; /* DIRTY_SINCE 0 = nothing written in the open transaction */
static sqlite3_stmt *S_UPSERT, *S_TEXT_DEL, *S_TEXT_INS;
static buf_t TEXT_BUF;

static void process(const char *rel, size_t reln, int64_t size, int64_t mtime, const char *local) {
    entry_t *k = table_upsert(&KNOWN_T, rel, reln);
    bool existed = k->flags & KNOWN;
    k->flags |= SEEN;
    COUNT++;
    /* commit every 5000 files, and once a write has waited for 200 ms (see the Go program) */
    if (COUNT % 5000 == 0 || (DIRTY_SINCE > 0 && now_secs() - DIRTY_SINCE > 0.2)) {
        DIRTY_SINCE = 0;
        exec("COMMIT; BEGIN");
        fprintf(stderr, "  %llu files (%.0fs)\n", (unsigned long long)COUNT, now_secs() - T0);
    }
    if (existed && k->size == size && k->mtime == mtime) {
        ST.same++;
        return;
    }
    /* name, lower-case extension (".bashrc" has none), top folder */
    const char *slash = last_byte(rel, '/', reln);
    const char *name = slash ? slash + 1 : rel;
    size_t namen = reln - (size_t)(name - rel);
    const char *dot = last_byte(name, '.', namen);
    char ext[256];
    size_t extn = 0;
    if (dot && dot > name) {
        extn = namen - (size_t)(dot - name) - 1;
        if (extn >= sizeof ext) extn = sizeof ext - 1;
        for (size_t i = 0; i < extn; i++) {
            char c = dot[1 + i];
            ext[i] = (c >= 'A' && c <= 'Z') ? (char)(c + 32) : c; /* ASCII only: see ports/README.md */
        }
    }
    ext[extn] = 0;
    const char *first = memchr(rel, '/', reln);
    size_t topn = first ? (size_t)(first - rel) : 0;

    char hex[65];
    bool have_hash = false;
    if (local && O.hash_min >= 0 && size >= O.hash_min) {
        if (sha256_file(local, hex) == 0) have_hash = true;
        else {
            ST.errors++;
            fprintf(stderr, "hash error %.*s: %s\n", (int)reln, rel, strerror(errno));
        }
    }
    if (DIRTY_SINCE == 0) DIRTY_SINCE = now_secs();
    sqlite3_stmt *s = S_UPSERT;
    sqlite3_bind_int64(s, 1, SID);
    sqlite3_bind_text(s, 2, rel, (int)reln, SQLITE_STATIC);
    sqlite3_bind_text(s, 3, name, (int)namen, SQLITE_STATIC);
    sqlite3_bind_text(s, 4, ext, (int)extn, SQLITE_STATIC);
    sqlite3_bind_int64(s, 5, size);
    sqlite3_bind_int64(s, 6, mtime);
    sqlite3_bind_text(s, 7, kind_of(ext, extn), -1, SQLITE_STATIC);
    sqlite3_bind_text(s, 8, rel, (int)topn, SQLITE_STATIC);
    if (have_hash) sqlite3_bind_text(s, 9, hex, 64, SQLITE_STATIC);
    else sqlite3_bind_null(s, 9);
    if (sqlite3_step(s) != SQLITE_ROW) die("%s", sqlite3_errmsg(g_db));
    int64_t id = sqlite3_column_int64(s, 0);
    sqlite3_reset(s);
    sqlite3_clear_bindings(s);
    if (existed) ST.changed++;
    else ST.new_++;
    if (local && O.text && text_allowed(rel, reln)) {
        read_text(local, ext, extn, &TEXT_BUF);
        if (TEXT_BUF.n > 0) {
            sqlite3_bind_int64(S_TEXT_DEL, 1, id);
            step_done(S_TEXT_DEL);
            sqlite3_bind_int64(S_TEXT_INS, 1, id);
            sqlite3_bind_text(S_TEXT_INS, 2, rel, (int)reln, SQLITE_STATIC);
            sqlite3_bind_text(S_TEXT_INS, 3, TEXT_BUF.p, (int)TEXT_BUF.n, SQLITE_STATIC);
            step_done(S_TEXT_INS);
        }
    }
}

static int cmp_names(const void *a, const void *b) {
    return strcmp(*(char *const *)a, *(char *const *)b); /* strcmp compares unsigned bytes: Go's order */
}

/* depth first, entries in byte order: the order of Go's filepath.WalkDir, so rows get the same ids */
static void walk(buf_t *path) {
    DIR *d = opendir(path->p);
    if (!d) {
        ST.errors++;
        fprintf(stderr, "read error %s: %s\n", path->p, strerror(errno));
        return;
    }
    char **names = NULL;
    size_t n = 0, cap = 0;
    struct dirent *de;
    errno = 0;
    while ((de = readdir(d))) {
        if (de->d_name[0] == '.' && (!de->d_name[1] || (de->d_name[1] == '.' && !de->d_name[2]))) continue;
        if (n == cap) {
            cap = cap ? cap * 2 : 64;
            names = xrealloc(names, cap * sizeof *names);
        }
        /* the type rides along after the name's NUL, so sorting moves both */
        size_t l = strlen(de->d_name);
        names[n] = xmalloc(l + 2);
        memcpy(names[n], de->d_name, l + 1);
        names[n][l + 1] = (char)de->d_type;
        n++;
    }
    if (errno) {
        ST.errors++;
        fprintf(stderr, "read error %s: %s\n", path->p, strerror(errno));
    }
    closedir(d);
    qsort(names, n, sizeof *names, cmp_names);
    size_t base = path->n;
    for (size_t i = 0; i < n; i++) {
        char *name = names[i];
        size_t l = strlen(name);
        unsigned char t = (unsigned char)name[l + 1];
        path->n = base;
        if (base > 0 && path->p[base - 1] != '/') buf_put(path, "/", 1);
        buf_put(path, name, l);
        struct stat st;
        if (t == DT_UNKNOWN) {
            if (lstat(path->p, &st) != 0) { free(name); continue; }
            t = S_ISDIR(st.st_mode) ? DT_DIR : S_ISLNK(st.st_mode) ? DT_LNK : DT_REG;
        }
        if (t == DT_DIR) {
            bool skip = skip_dir(name) ||
                        (HOME_MODE && (name[0] == '.' || in_list(HOME_SKIP_DIRS, name, l) ||
                                       (base == strlen(O.root) && (!strcmp(name, "Library") || !strcmp(name, "AppData")))));
            if (!skip) walk(path);
            free(name);
            continue;
        }
        if (skip_file(name)) { free(name); continue; }
        /* like a normal stat: follow links to files, ignore links to folders */
        int rc = t == DT_LNK ? stat(path->p, &st) : lstat(path->p, &st);
        if (rc != 0) {
            ST.errors++;
            fprintf(stderr, "stat error %s: %s\n", path->p, strerror(errno));
            free(name);
            continue;
        }
        if (S_ISREG(st.st_mode))
            process(path->p + ROOT_LEN, path->n - ROOT_LEN, (int64_t)st.st_size, (int64_t)st.st_mtime,
                    placeholder(&st) ? NULL : path->p);
        free(name);
    }
    path->n = base;
    path->p[base] = 0;
    free(names);
}

static void run(void) {
    T0 = now_secs();
    struct stat rst;
    if (stat(O.root, &rst) != 0 || !S_ISDIR(rst.st_mode)) die("not a folder: %s", O.root);
    if (sqlite3_open(O.db, &g_db) != SQLITE_OK) die("%s: %s", O.db, sqlite3_errmsg(g_db));
    sqlite3_busy_timeout(g_db, 30000);
    exec("PRAGMA foreign_keys=OFF"); /* compiled with rusqlite's flags, which turn them on */
    exec(DDL);
    sqlite3_stmt *s = prepare("SELECT value FROM meta WHERE key='schema_version'");
    if (sqlite3_step(s) == SQLITE_ROW && atoi((const char *)sqlite3_column_text(s, 0)) > SCHEMA_VERSION)
        die("this index uses schema %s, newer than this program supports (%d)", sqlite3_column_text(s, 0), SCHEMA_VERSION);
    sqlite3_finalize(s);

    unsigned char rnd[16];
    if (getentropy(rnd, sizeof rnd) != 0) die("getentropy: %s", strerror(errno));
    char disk_id[33];
    for (int i = 0; i < 16; i++) sprintf(disk_id + 2 * i, "%02x", rnd[i]);
    s = prepare("INSERT OR REPLACE INTO meta VALUES ('schema_version', ?)");
    sqlite3_bind_text(s, 1, "1", -1, SQLITE_STATIC);
    step_done(s), sqlite3_finalize(s);
    s = prepare("INSERT OR REPLACE INTO meta VALUES ('sidecar_version', ?)");
    sqlite3_bind_text(s, 1, SIDECAR_VERSION, -1, SQLITE_STATIC);
    step_done(s), sqlite3_finalize(s);
    s = prepare("INSERT OR IGNORE INTO meta VALUES ('disk_id', ?)");
    sqlite3_bind_text(s, 1, disk_id, -1, SQLITE_STATIC);
    step_done(s), sqlite3_finalize(s);
    s = prepare("INSERT INTO sources(label, root, host) VALUES (?,?,?)\n"
                "\t\tON CONFLICT(label) DO UPDATE SET root=excluded.root, host=excluded.host");
    sqlite3_bind_text(s, 1, O.label, -1, SQLITE_STATIC);
    sqlite3_bind_text(s, 2, O.root, -1, SQLITE_STATIC);
    sqlite3_bind_text(s, 3, O.host, -1, SQLITE_STATIC);
    step_done(s), sqlite3_finalize(s);
    s = prepare("SELECT id FROM sources WHERE label=?");
    sqlite3_bind_text(s, 1, O.label, -1, SQLITE_STATIC);
    if (sqlite3_step(s) != SQLITE_ROW) die("source not found");
    SID = sqlite3_column_int64(s, 0);
    sqlite3_finalize(s);

    KNOWN_T.cap = 1024;
    KNOWN_T.e = xmalloc(sizeof(entry_t) * KNOWN_T.cap);
    memset(KNOWN_T.e, 0, sizeof(entry_t) * KNOWN_T.cap);
    s = prepare("SELECT path, size, mtime FROM files WHERE source_id=?");
    sqlite3_bind_int64(s, 1, SID);
    while (sqlite3_step(s) == SQLITE_ROW) {
        entry_t *e = table_upsert(&KNOWN_T, (const char *)sqlite3_column_text(s, 0), (size_t)sqlite3_column_bytes(s, 0));
        e->size = sqlite3_column_int64(s, 1);
        e->mtime = sqlite3_column_int64(s, 2);
        e->flags = KNOWN;
    }
    sqlite3_finalize(s);

    size_t rl = strlen(O.root);
    ROOT_LEN = (rl > 0 && O.root[rl - 1] == '/') ? rl : rl + 1;
    HOME_MODE = is_home(O.root);
    S_UPSERT = prepare(UPSERT_FILE);
    S_TEXT_DEL = prepare("DELETE FROM file_text WHERE rowid=?");
    S_TEXT_INS = prepare("INSERT INTO file_text(rowid, path, body) VALUES (?,?,?)");

    exec("BEGIN");
    buf_t path = {0};
    buf_put(&path, O.root, rl);
    walk(&path);
    free(path.p);

    sqlite3_stmt *d1 = prepare("DELETE FROM file_text WHERE rowid IN (SELECT id FROM files WHERE source_id=? AND path=?)");
    sqlite3_stmt *d2 = prepare("DELETE FROM files WHERE source_id=? AND path=?");
    for (size_t i = 0; i < KNOWN_T.cap; i++) {
        entry_t *e = &KNOWN_T.e[i];
        if (!e->key || !(e->flags & KNOWN) || (e->flags & SEEN)) continue;
        sqlite3_bind_int64(d1, 1, SID);
        sqlite3_bind_text(d1, 2, e->key, (int)e->len, SQLITE_STATIC);
        step_done(d1);
        sqlite3_bind_int64(d2, 1, SID);
        sqlite3_bind_text(d2, 2, e->key, (int)e->len, SQLITE_STATIC);
        step_done(d2);
        ST.removed++;
    }
    sqlite3_finalize(d1);
    sqlite3_finalize(d2);

    char cleaned[64], key[512] = "";
    snprintf(cleaned, sizeof cleaned, "notext_dirs:%lld", (long long)SID);
    for (const char **p = NO_TEXT_DIRS; *p; p++) {
        if (p != NO_TEXT_DIRS) strcat(key, "/");
        strcat(key, *p);
    }
    s = prepare("SELECT value FROM meta WHERE key=?");
    sqlite3_bind_text(s, 1, cleaned, -1, SQLITE_STATIC);
    bool done = sqlite3_step(s) == SQLITE_ROW && strcmp((const char *)sqlite3_column_text(s, 0), key) == 0;
    sqlite3_finalize(s);
    if (O.text && !done) {
        s = prepare("DELETE FROM file_text WHERE rowid IN (SELECT id FROM files WHERE source_id=? AND "
                    "(path LIKE ? ESCAPE '\\' OR path LIKE ? ESCAPE '\\'))");
        for (const char **p = NO_TEXT_DIRS; *p; p++) {
            buf_t like = {0}, like2 = {0};
            for (const char *c = *p; *c; c++) {
                if (*c == '\\' || *c == '%' || *c == '_') buf_put(&like, "\\", 1);
                buf_put(&like, c, 1);
            }
            buf_put(&like, "/%", 2);
            buf_put(&like2, "%/", 2);
            buf_put(&like2, like.p, like.n);
            sqlite3_bind_int64(s, 1, SID);
            sqlite3_bind_text(s, 2, like.p, (int)like.n, SQLITE_TRANSIENT);
            sqlite3_bind_text(s, 3, like2.p, (int)like2.n, SQLITE_TRANSIENT);
            step_done(s);
            free(like.p), free(like2.p);
        }
        sqlite3_finalize(s);
        s = prepare("INSERT OR REPLACE INTO meta VALUES (?, ?)");
        sqlite3_bind_text(s, 1, cleaned, -1, SQLITE_STATIC);
        sqlite3_bind_text(s, 2, key, -1, SQLITE_STATIC);
        step_done(s), sqlite3_finalize(s);
    }
    s = prepare("UPDATE sources SET scanned_at=? WHERE id=?");
    sqlite3_bind_int64(s, 1, (int64_t)time(NULL));
    sqlite3_bind_int64(s, 2, SID);
    step_done(s), sqlite3_finalize(s);
    sqlite3_finalize(S_UPSERT), sqlite3_finalize(S_TEXT_DEL), sqlite3_finalize(S_TEXT_INS);
    exec("COMMIT");

    s = prepare("SELECT COUNT(*), COALESCE(SUM(size),0) FROM files WHERE source_id=?");
    sqlite3_bind_int64(s, 1, SID);
    if (sqlite3_step(s) == SQLITE_ROW) ST.total = sqlite3_column_int64(s, 0), ST.size = sqlite3_column_int64(s, 1);
    sqlite3_finalize(s);
    ST.secs = now_secs() - T0;
}

/* ------------------------------------------------------------------ the command line */

int main(int argc, char **argv) {
    const char *db = NULL, *source = NULL, *folder = NULL;
    double hash_mb = -1;
    bool no_text = false, with_text = false;
    for (int i = 1; i < argc; i++) {
        const char *a = argv[i];
        if (a[0] != '-' || !a[1]) {
            if (folder) die("usage: sidecar-scan-c <folder> [--db index.db] [--source name]");
            folder = a;
            continue;
        }
        while (*a == '-') a++;
        const char *eq = strchr(a, '=');
        size_t nl = eq ? (size_t)(eq - a) : strlen(a);
#define FLAG(s) (nl == strlen(s) && memcmp(a, s, nl) == 0)
#define VALUE() (eq ? eq + 1 : (i + 1 < argc ? argv[++i] : (die("missing value for %s", argv[i]), "")))
        if (FLAG("db")) db = VALUE();
        else if (FLAG("source")) source = VALUE();
        else if (FLAG("hash-min-mb")) hash_mb = atof(VALUE());
        else if (FLAG("no-text")) no_text = true;
        else if (FLAG("text")) with_text = true;
        else if (FLAG("version")) { printf("sidecar-scan-c %s\n", SIDECAR_VERSION); return 0; }
        else die("unknown flag %s\nusage: sidecar-scan-c <folder> [--db index.db] [--source name] [--hash-min-mb N] [--no-text] [--text]", argv[i]);
    }
    if (!folder) die("usage: sidecar-scan-c <folder> [--db index.db] [--source name]");
    char *root = abs_clean(folder);
    if (!db) {
        char *p = xmalloc(strlen(root) + 32);
        sprintf(p, "%s/.sidecar/index.db", strcmp(root, "/") ? root : "");
        db = p;
    }
    if (!source) {
        const char *s = strrchr(root, '/');
        source = (s && s[1]) ? s + 1 : "/";
    }
    static char host[256];
    if (gethostname(host, sizeof host) != 0) host[0] = 0;
    O = (options_t){root, db, source, host, hash_mb >= 0 ? (int64_t)(hash_mb * 1024 * 1024) : -1,
                    !no_text && (with_text || !is_home(root))};
    g_io = xmalloc(IO_BUF);
    if (!getenv("SIDECAR_SCAN_FOREGROUND")) background(); /* set only by ports/bench */
    run();
    printf("[%s] %lld files, %.1f GB | new %lld, changed %lld, same %lld, removed %lld, errors %lld | %.0fs\n", O.label,
           (long long)ST.total, ST.size / 1e9, (long long)ST.new_, (long long)ST.changed, (long long)ST.same,
           (long long)ST.removed, (long long)ST.errors, ST.secs);
    sqlite3_close(g_db);
    return 0;
}
