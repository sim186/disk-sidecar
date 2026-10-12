// Package phone catalogues the files of a phone. A phone cannot carry a sidecar the way a disk
// does, so its catalog lives on this computer, in ~/.sidecar/phones, and is always read only.
//
// Android is read through adb (USB debugging on, adb installed): nothing is copied to the phone.
// An iPhone shows only its camera roll (DCIM), through ifuse and libimobiledevice; the apps' data
// is sandboxed by Apple and out of reach.
package phone

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sim186/disk-sidecar/internal/idx"
	"github.com/sim186/disk-sidecar/internal/scan"
)

// Device is a phone that is connected now.
type Device struct {
	Platform string `json:"platform"` // "android" or "ios"
	Serial   string `json:"serial"`
	Name     string `json:"name"`
	State    string `json:"state"` // "ready", or why it cannot be read yet
	Hint     string `json:"hint,omitempty"`
}

// Dir is where the catalogs of phones are kept.
func Dir() (string, error) {
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return "", fmt.Errorf("no home folder")
	}
	return filepath.Join(h, ".sidecar", "phones"), nil
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// DBPath is the catalog of one device.
func DBPath(d Device) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, d.Platform+"-"+unsafeName.ReplaceAllString(d.Serial, "_")+".db"), nil
}

func run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}

// Detect lists the phones connected now. Missing tools are not an error: they only mean that
// kind of phone cannot be seen.
func Detect() []Device {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	var out []Device
	if _, err := exec.LookPath("adb"); err == nil {
		if o, err := run(ctx, "adb", "devices", "-l"); err == nil {
			out = append(out, parseADBDevices(o)...)
		}
	}
	if _, err := exec.LookPath("idevice_id"); err == nil {
		if o, err := run(ctx, "idevice_id", "-l"); err == nil {
			for _, udid := range strings.Fields(o) {
				name, _ := run(ctx, "ideviceinfo", "-u", udid, "-k", "DeviceName")
				d := Device{Platform: "ios", Serial: udid, Name: strings.TrimSpace(name), State: "ready"}
				if d.Name == "" {
					d.Name = "iPhone"
				}
				if _, err := exec.LookPath("ifuse"); err != nil {
					d.State, d.Hint = "needs ifuse", "Install ifuse (macFUSE on a Mac) to read the photos of an iPhone."
				}
				out = append(out, d)
			}
		}
	}
	return out
}

// parseADBDevices reads the output of "adb devices -l".
func parseADBDevices(o string) []Device {
	var out []Device
	for _, line := range strings.Split(o, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] == "List" || strings.HasPrefix(f[0], "*") {
			continue
		}
		d := Device{Platform: "android", Serial: f[0], Name: "Android phone"}
		for _, kv := range f[2:] {
			if v, ok := strings.CutPrefix(kv, "model:"); ok {
				d.Name = strings.ReplaceAll(v, "_", " ")
			}
		}
		switch f[1] {
		case "device":
			d.State = "ready"
		case "unauthorized":
			d.State, d.Hint = "unlock the phone", "Unlock the phone and accept \"Allow USB debugging\", then try again."
		default:
			d.State, d.Hint = f[1], "The phone is not ready: reconnect the cable."
		}
		out = append(out, d)
	}
	return out
}

// quote makes a path safe inside the single quotes of the phone's shell.
func quote(p string) string { return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'" }

// parseStat reads lines "size|mtime|/path" as printed by stat -c '%s|%Y|%n'. root is removed
// from the front of the path.
func parseStat(line, root string) (scan.Entry, bool) {
	a, rest, ok := strings.Cut(line, "|")
	if !ok {
		return scan.Entry{}, false
	}
	b, p, ok := strings.Cut(rest, "|")
	if !ok {
		return scan.Entry{}, false
	}
	size, err1 := strconv.ParseInt(a, 10, 64)
	mtime, err2 := strconv.ParseInt(b, 10, 64)
	rel, under := strings.CutPrefix(strings.TrimRight(p, "\r"), strings.TrimRight(root, "/")+"/")
	if err1 != nil || err2 != nil || !under || rel == "" {
		return scan.Entry{}, false
	}
	return scan.Entry{Path: rel, Size: size, Mtime: mtime}, true
}

// androidRoot is the shared storage of the phone: photos, downloads, documents, app media.
const androidRoot = "/sdcard"

// ScanAndroid catalogues the shared storage of an Android phone through adb.
func ScanAndroid(d Device, o scan.Options) (scan.Stats, error) {
	o.Root = "phone:android:" + d.Serial
	o.Entries = func(yield func(scan.Entry) error) error {
		cmd := exec.Command("adb", "-s", d.Serial, "shell",
			"find "+quote(androidRoot+"/")+" -type f -exec stat -c '%s|%Y|%n' {} + 2>/dev/null")
		pipe, err := cmd.StdoutPipe()
		if err != nil {
			return err
		}
		if err := cmd.Start(); err != nil {
			return err
		}
		sc := bufio.NewScanner(pipe)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		var yerr error
		n := 0
		for sc.Scan() && yerr == nil {
			if e, ok := parseStat(sc.Text(), androidRoot); ok {
				n++
				yerr = yield(e)
			}
		}
		if yerr != nil { // stop adb and report the real reason
			cmd.Process.Kill()
			io.Copy(io.Discard, pipe)
			cmd.Wait()
			return yerr
		}
		werr := cmd.Wait()
		if n == 0 { // a locked or unplugged phone must not look like a phone that was emptied
			return fmt.Errorf("adb found no files on the phone (is it unlocked, and is USB debugging allowed?) %v", werr)
		}
		if werr != nil && o.Log != nil {
			fmt.Fprintf(o.Log, "adb: %v (some folders could not be read)\n", werr)
		}
		return nil
	}
	return scan.Run(o)
}

// ScanIOS mounts the camera roll of an iPhone read only, catalogues it and unmounts it.
func ScanIOS(d Device, o scan.Options) (scan.Stats, error) {
	mnt, err := os.MkdirTemp("", "sidecar-iphone-")
	if err != nil {
		return scan.Stats{}, err
	}
	defer os.Remove(mnt)
	if out, err := exec.Command("ifuse", "-u", d.Serial, mnt).CombinedOutput(); err != nil {
		return scan.Stats{}, fmt.Errorf("ifuse could not open the iPhone (unlock it and accept \"Trust\"): %s", strings.TrimSpace(string(out)))
	}
	defer unmount(mnt)
	o.Root = filepath.Join(mnt, "DCIM")
	st, err := scan.Run(o)
	if err != nil {
		return st, err
	}
	return st, nil
}

func unmount(mnt string) {
	if _, err := exec.LookPath("fusermount"); err == nil {
		exec.Command("fusermount", "-u", mnt).Run()
		return
	}
	exec.Command("umount", mnt).Run()
}

// Scan catalogues a device into its catalog under Dir.
func Scan(d Device, log io.Writer, progress func(int)) (scan.Stats, error) {
	if d.State != "ready" {
		if d.Hint != "" {
			return scan.Stats{}, fmt.Errorf("%s: %s", d.Name, d.Hint)
		}
		return scan.Stats{}, fmt.Errorf("%s is not ready (%s)", d.Name, d.State)
	}
	db, err := DBPath(d)
	if err != nil {
		return scan.Stats{}, err
	}
	if err := os.MkdirAll(filepath.Dir(db), 0o755); err != nil {
		return scan.Stats{}, err
	}
	o := scan.Options{DB: db, Label: d.Name, Host: "phone", HashMin: -1, Progress: progress, Log: log}
	var st scan.Stats
	switch d.Platform {
	case "android":
		st, err = ScanAndroid(d, o)
	case "ios":
		st, err = ScanIOS(d, o)
	default:
		return scan.Stats{}, fmt.Errorf("unknown platform %q", d.Platform)
	}
	if err == nil { // the catalog remembers which device it describes (copy and release need the real serial)
		if mdb, merr := idx.Open(db); merr == nil {
			mdb.Exec(`INSERT OR REPLACE INTO meta VALUES ('phone_serial', ?)`, d.Serial)
			mdb.Exec(`INSERT OR REPLACE INTO meta VALUES ('phone_platform', ?)`, d.Platform)
			mdb.Close()
		}
	}
	return st, err
}

// Identity reads which device a phone catalog describes; ok is false for a catalog made before
// this was recorded (scan the phone again).
func Identity(dbPath string) (platform, serial string, ok bool) {
	db, err := idx.OpenRO(dbPath)
	if err != nil {
		return "", "", false
	}
	defer db.Close()
	db.QueryRow(`SELECT value FROM meta WHERE key='phone_platform'`).Scan(&platform)
	db.QueryRow(`SELECT value FROM meta WHERE key='phone_serial'`).Scan(&serial)
	return platform, serial, platform != "" && serial != ""
}

// RemotePath turns a path in the catalog into the path on the phone. rel must already be clean.
func RemotePath(rel string) string { return androidRoot + "/" + rel }

var hexSum = regexp.MustCompile(`^[0-9a-f]{64}$`)

// RemoteSHA is the SHA-256 of a file as the phone itself reads it.
func RemoteSHA(serial, remote string) (string, error) {
	out, err := exec.Command("adb", "-s", serial, "shell", "sha256sum "+quote(remote)).Output()
	f := strings.Fields(string(out))
	if err != nil || len(f) == 0 || !hexSum.MatchString(f[0]) {
		return "", fmt.Errorf("the phone could not read %s (is it unlocked?)", remote)
	}
	return f[0], nil
}

// Pull copies a file from the phone to local, which is overwritten.
func Pull(serial, remote, local string) error {
	if out, err := exec.Command("adb", "-s", serial, "pull", "-a", remote, local).CombinedOutput(); err != nil {
		return fmt.Errorf("adb pull %s: %s", remote, strings.TrimSpace(string(out)))
	}
	return nil
}

// Remove deletes one file on the phone. There is no trash: it cannot be undone.
func Remove(serial, remote string) error {
	if out, err := exec.Command("adb", "-s", serial, "shell", "rm -f "+quote(remote)).CombinedOutput(); err != nil {
		return fmt.Errorf("adb rm %s: %s", remote, strings.TrimSpace(string(out)))
	}
	return nil
}

// Find returns the connected device with this serial.
func Find(serial string) (Device, bool) {
	for _, d := range Detect() {
		if d.Serial == serial {
			return d, true
		}
	}
	return Device{}, false
}
