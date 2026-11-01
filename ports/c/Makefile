# The C port of `sidecar scan`. It is compiled against the very SQLite source the Rust port
# bundles (rusqlite's libsqlite3-sys), with the same compile options, so the two differ only in
# the code around SQLite. Run `cargo fetch` in ../rust first (the benchmark does it).
#
#   make                         CommonCrypto SHA-256 on macOS
#   make SHA=portable            the plain C SHA-256 instead
#   make SQLITE=system           link the system's libsqlite3 instead of the bundled source

CC      ?= cc
CFLAGS  ?= -O3
CFLAGS  += -std=c11 -Wall -Wextra -Wno-unused-parameter
VERSION := $(shell cat ../../internal/assets/VERSION)
OUT     := build/sidecar-scan-c

# the same -D flags as libsqlite3-sys/build.rs (bundled feature, no extras)
SQLITE_DEFS := -DSQLITE_CORE -DSQLITE_DEFAULT_FOREIGN_KEYS=1 -DSQLITE_ENABLE_API_ARMOR \
  -DSQLITE_ENABLE_COLUMN_METADATA -DSQLITE_ENABLE_DBSTAT_VTAB -DSQLITE_ENABLE_FTS3 \
  -DSQLITE_ENABLE_FTS3_PARENTHESIS -DSQLITE_ENABLE_FTS5 -DSQLITE_ENABLE_JSON1 \
  -DSQLITE_ENABLE_LOAD_EXTENSION=1 -DSQLITE_ENABLE_MEMORY_MANAGEMENT -DSQLITE_ENABLE_RTREE -DSQLITE_ENABLE_STAT4 -DSQLITE_SOUNDEX \
  -DSQLITE_THREADSAFE=1 -DSQLITE_USE_URI -DHAVE_USLEEP=1 -DHAVE_ISNAN -DHAVE_LOCALTIME_R -DNDEBUG

ifeq ($(SQLITE),system)
  SQLITE_DIR :=
  SQLITE_OBJ :=
  LDLIBS     += -lsqlite3
else
  SQLITE_DIR ?= $(shell cd ../rust && cargo metadata --format-version 1 2>/dev/null | \
    python3 -c 'import json,sys,os; m=json.load(sys.stdin); print(next(os.path.dirname(p["manifest_path"]) for p in m["packages"] if p["name"]=="libsqlite3-sys")+"/sqlite3")')
  SQLITE_OBJ := build/sqlite3.o
  CPPFLAGS   += -I$(SQLITE_DIR)
endif

ifeq ($(SHA),portable)
  CPPFLAGS += -DSIDECAR_PORTABLE_SHA256
endif

LDLIBS += -lpthread -lm

$(OUT): sidecar_scan.c sha256.c sha256.h $(SQLITE_OBJ) | build
	$(CC) $(CFLAGS) $(CPPFLAGS) -DSIDECAR_VERSION='"$(VERSION)"' -o $@ sidecar_scan.c sha256.c $(SQLITE_OBJ) $(LDLIBS)
	strip $@

build/sqlite3.o: | build
	@test -f "$(SQLITE_DIR)/sqlite3.c" || { echo "SQLite source not found: run 'cargo fetch' in ports/rust"; exit 1; }
	$(CC) -O3 -w $(SQLITE_DEFS) -c -o $@ $(SQLITE_DIR)/sqlite3.c

build:
	mkdir -p build

clean:
	rm -rf build

.PHONY: clean
