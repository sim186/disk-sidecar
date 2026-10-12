package tests

import (
	"bytes"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sim186/disk-sidecar/internal/idx"
)

// A fake adb stands in for an Android phone.
func TestPhoneScanAndroid(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake adb is a shell script")
	}
	home, tools := t.TempDir(), t.TempDir()
	write(t, filepath.Join(tools, "adb"), `#!/bin/sh
case "$*" in
  "devices -l") printf 'List of devices attached\nSER1 device usb:1 model:Pixel_8 transport_id:1\n\n' ;;
  *"find"*) printf '100|1700000000|/sdcard/DCIM/Camera/a.jpg\n2000|1700000001|/sdcard/Download/b.pdf\n' ;;
esac
`)
	os.Chmod(filepath.Join(tools, "adb"), 0o755)
	env := append(os.Environ(), "HOME="+home, "USERPROFILE="+home, "PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	do := func(args ...string) string {
		cmd := exec.Command(bin, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	if out := do("phone"); !strings.Contains(out, "Pixel 8") || !strings.Contains(out, "ready") {
		t.Fatalf("phone list: %s", out)
	}
	if out := do("phone", "scan", "SER1"); !strings.Contains(out, "2 files") {
		t.Fatalf("scan: %s", out)
	}
	db, err := idx.OpenRO(filepath.Join(home, ".sidecar", "phones", "android-SER1.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	var top sql.NullString
	if err := db.QueryRow(`SELECT COUNT(*), MIN(top) FROM files`).Scan(&n, &top); err != nil || n != 2 || top.String != "DCIM" {
		t.Fatalf("files: n=%d top=%q err=%v", n, top.String, err)
	}
}

const fakeADB = `#!/usr/bin/env python3
import hashlib, os, shutil, sys
root = os.environ["FAKE_PHONE"]
a = sys.argv[1:]
def real(p): return os.path.join(root, p[len("/sdcard/"):])
def unq(s): return s.strip("'").replace("'\\''", "'")
if a[:2] == ["devices", "-l"]:
    print("List of devices attached\nSER1 device usb:1 model:Pixel_8 transport_id:1\n")
    sys.exit(0)
a = a[2:]  # -s SER
if a[0] == "pull":
    shutil.copyfile(real(a[-2]), a[-1]); sys.exit(0)
cmd = a[1]
if cmd.startswith("find "):
    for d, _, fs in os.walk(root):
        for f in fs:
            p = os.path.join(d, f); st = os.stat(p)
            print("%d|%d|/sdcard/%s" % (st.st_size, st.st_mtime, os.path.relpath(p, root)))
elif cmd.startswith("sha256sum "):
    p = real(unq(cmd[len("sha256sum "):]))
    if not os.path.exists(p): sys.exit(1)
    print(hashlib.sha256(open(p, "rb").read()).hexdigest() + "  " + p)
elif cmd.startswith("rm -f "):
    p = real(unq(cmd[len("rm -f "):]))
    if os.path.exists(p): os.remove(p)
`

func TestPhoneCopyAndRelease(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake adb is a script")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("needs python3 for the fake adb")
	}
	tools, fake := t.TempDir(), t.TempDir()
	write(t, filepath.Join(tools, "adb"), fakeADB)
	os.Chmod(filepath.Join(tools, "adb"), 0o755)
	write(t, fake+"/DCIM/a.jpg", blob(300_000, 11))
	write(t, fake+"/DCIM/b.jpg", blob(300_000, 12))
	write(t, fake+"/Download/c.bin", blob(1000, 13))
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_PHONE", fake)

	s := newServer(t)
	s.waitStartup()
	home := filepath.Dir(s.vol)
	cmd := exec.Command(bin, "phone", "scan", "SER1")
	cmd.Env = append(os.Environ(), "HOME="+home, "USERPROFILE="+home)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("scan: %v %s", err, out)
	}
	item := func(p string) map[string]string { return map[string]string{"source": "Pixel 8", "path": p} }
	rel := func(dry bool, confirm string, items ...any) (string, map[string]any, string) {
		return s.job("/api/phone/release", map[string]any{"items": items, "dry": dry, "confirm": confirm})
	}

	// nothing is backed up yet: nothing may go
	if st, res, msg := rel(true, "", item("DCIM")); st != "done" || res["release"] != 0.0 || res["kept"] != 2.0 {
		t.Fatalf("unbacked: %s %v %s", st, res, msg)
	}

	// copy one photo to a disk
	if st, res, msg := s.job("/api/phone/copy", map[string]any{"items": []any{item("DCIM/a.jpg")}, "to": map[string]string{"source": "Disk B", "path": "phone"}}); st != "done" || res["files"] != 1.0 {
		t.Fatalf("copy: %s %v %s", st, res, msg)
	}
	want, _ := os.ReadFile(fake + "/DCIM/a.jpg")
	got, _ := os.ReadFile(s.B + "/phone/a.jpg")
	if !bytes.Equal(got, want) {
		t.Fatal("copy differs")
	}
	if !exists(fake + "/DCIM/a.jpg") {
		t.Fatal("copying removed the original")
	}
	if n := count(t, s.B+"/.sidecar/index.db", "select count(*) from files where path='phone/a.jpg' and sha256 is not null"); n != 1 {
		t.Fatal("the copy is not indexed")
	}
	if st, _, msg := s.job("/api/phone/copy", map[string]any{"items": []any{item("DCIM/a.jpg")}, "to": map[string]string{"source": "Disk B", "path": "phone"}}); st != "error" || !strings.Contains(msg, "not overwriting") {
		t.Fatalf("overwrite: %s %s", st, msg)
	}

	// a damaged copy on the disk proves nothing, whatever the index says
	os.WriteFile(s.B+"/phone/a.jpg", []byte(blob(300_000, 99)), 0o644)
	if st, res, _ := rel(true, "", item("DCIM/a.jpg")); st != "done" || res["release"] != 0.0 {
		t.Fatalf("damaged copy accepted: %v", res)
	}
	os.WriteFile(s.B+"/phone/a.jpg", want, 0o644)

	// dry run changes nothing; real needs the word; only the proven file goes
	if st, res, _ := rel(true, "", item("DCIM")); st != "done" || res["release"] != 1.0 || res["kept"] != 1.0 {
		t.Fatalf("dry: %v", res)
	}
	if !exists(fake + "/DCIM/a.jpg") {
		t.Fatal("dry run deleted")
	}
	if st, _, msg := rel(false, "DELETE", item("DCIM")); st != "error" || !strings.Contains(msg, "confirmation") {
		t.Fatalf("no word: %s %s", st, msg)
	}
	if st, res, msg := rel(false, "Let it go ", item("DCIM")); st != "done" || res["release"] != 1.0 {
		t.Fatalf("release: %s %v %s", st, res, msg)
	}
	if exists(fake+"/DCIM/a.jpg") || !exists(fake+"/DCIM/b.jpg") || !exists(s.B+"/phone/a.jpg") {
		t.Fatal("wrong files removed")
	}
}
