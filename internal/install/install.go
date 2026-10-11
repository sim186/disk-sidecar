// Package install puts the programs on a disk (<disk>/.sidecar/) and keeps them up to date,
// without ever touching the data that lives there.
package install

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/sim186/disk-sidecar/internal/assets"
	"github.com/sim186/disk-sidecar/internal/scan"
)

// Options for Run.
type Options struct {
	Disk, Label       string
	Scan, NoScan, Yes bool
	DryRun            bool
	From              []string // folders with the program built for other systems
	Out, Err          io.Writer
	In                *os.File
}

// What an update never overwrites or removes: the user's data.
var keep = map[string]bool{"index.db": true, "index.db.bak": true, "log.csv": true, "peers": true, "_quarantena": true, "MANIFEST": true}

var binRe = regexp.MustCompile(`^sidecar-(darwin|linux|windows)-(amd64|arm64)(\.exe)?$`)

func binName(goos, goarch string) string {
	n := "bin/sidecar-" + goos + "-" + goarch
	if goos == "windows" {
		n += ".exe"
	}
	return n
}

func read(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func sameContent(a, b string) bool {
	sa, err1 := os.Stat(a)
	sb, err2 := os.Stat(b)
	if err1 != nil || err2 != nil || sa.Size() != sb.Size() {
		return false
	}
	if os.SameFile(sa, sb) {
		return true
	}
	x, _ := os.ReadFile(a)
	y, _ := os.ReadFile(b)
	return bytes.Equal(x, y)
}

func copyFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".new"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	os.Remove(dst)
	return os.Rename(tmp, dst)
}

func ask(o Options, question string, def bool) bool {
	if o.Yes || o.In == nil {
		return def
	}
	if st, err := o.In.Stat(); err != nil || st.Mode()&os.ModeCharDevice == 0 {
		return def
	}
	opt := "Y/n"
	if !def {
		opt = "y/N"
	}
	fmt.Fprintf(o.Out, "%s [%s] ", question, opt)
	var a string
	fmt.Fscanln(o.In, &a)
	a = strings.ToLower(strings.TrimSpace(a))
	if a == "" {
		return def
	}
	return strings.HasPrefix(a, "y") || strings.HasPrefix(a, "s")
}

type item struct {
	rel, src string // src is a file on this computer
	mode     os.FileMode
}

func plan(o Options) ([]item, error) {
	var items []item
	web := assets.Disk()
	err := fs.WalkDir(web, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		tmp, err := os.CreateTemp("", "sidecar-asset-*")
		if err != nil {
			return err
		}
		b, _ := fs.ReadFile(web, p)
		tmp.Write(b)
		tmp.Close()
		mode := os.FileMode(0o644)
		if p == "sidecar" || strings.HasSuffix(p, ".command") {
			mode = 0o755
		}
		items = append(items, item{p, tmp.Name(), mode})
		return nil
	})
	if err != nil {
		return nil, err
	}
	// the programs: this one, and every build for another system found next to it or in --from
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if real, err := filepath.EvalSymlinks(self); err == nil {
		self = real
	}
	items = append(items, item{binName(runtime.GOOS, runtime.GOARCH), self, 0o755})
	have := map[string]bool{binName(runtime.GOOS, runtime.GOARCH): true}
	dirs := append([]string{filepath.Dir(self), filepath.Join(filepath.Dir(self), "bin")}, o.From...)
	for _, dir := range dirs {
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			m := binRe.FindStringSubmatch(e.Name())
			if m == nil || e.IsDir() {
				continue
			}
			rel := "bin/" + e.Name()
			if !have[rel] {
				have[rel] = true
				items = append(items, item{rel, filepath.Join(dir, e.Name()), 0o755})
			}
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].rel < items[j].rel })
	return items, nil
}

func isDiskRoot(disk string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(disk, filepath.VolumeName(disk)+`\`)
	}
	parent := filepath.Dir(disk)
	for _, p := range []string{"/Volumes", "/media", "/mnt", "/run/media"} {
		if parent == p || filepath.Dir(parent) == p {
			return true
		}
	}
	return disk == "/"
}

// Run installs or updates Sidecar on o.Disk.
func Run(o Options) error {
	if o.Out == nil {
		o.Out = io.Discard
	}
	disk, err := filepath.Abs(o.Disk)
	if err != nil {
		return err
	}
	if !dirExists(disk) {
		return fmt.Errorf("not a folder: %s. Is the disk plugged in?", disk)
	}
	if !isDiskRoot(disk) && !o.Yes && !o.DryRun {
		if !ask(o, fmt.Sprintf("%s does not look like the root of a disk. Sidecar will live in %s. Continue?", disk, filepath.Join(disk, ".sidecar")), false) {
			return fmt.Errorf("cancelled")
		}
	}
	target := filepath.Join(disk, ".sidecar")
	oldV := read(filepath.Join(target, "VERSION"))
	fmt.Fprintf(o.Out, "Disk Sidecar %s -> %s", assets.Version, target)
	if oldV != "" {
		fmt.Fprintf(o.Out, "  (replacing %s)\n", oldV)
	} else {
		fmt.Fprintln(o.Out, "  (new install)")
	}
	items, err := plan(o)
	if err != nil {
		return err
	}
	defer func() {
		for _, it := range items {
			if strings.HasPrefix(it.src, os.TempDir()) && strings.Contains(it.src, "sidecar-asset-") {
				os.Remove(it.src)
			}
		}
	}()
	items = append(items, item{rel: "VERSION"})
	var oldManifest []string
	for _, l := range strings.Split(read(filepath.Join(target, "MANIFEST")), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			oldManifest = append(oldManifest, l)
		}
	}
	var rels []string
	changed := 0
	for _, it := range items {
		rels = append(rels, it.rel)
		dst := filepath.Join(target, filepath.FromSlash(it.rel))
		if it.rel == "VERSION" {
			if read(dst) == assets.Version {
				continue
			}
			changed++
			fmt.Fprintf(o.Out, "%s  VERSION\n", verb(o.DryRun, "copy"))
			if !o.DryRun {
				os.MkdirAll(target, 0o755)
				os.WriteFile(dst, []byte(assets.Version+"\n"), 0o644)
			}
			continue
		}
		if sameContent(it.src, dst) {
			continue
		}
		changed++
		fmt.Fprintf(o.Out, "%s  %s\n", verb(o.DryRun, "copy"), it.rel)
		if !o.DryRun {
			if err := copyFile(it.src, dst, it.mode); err != nil {
				return fmt.Errorf("%s: %w", it.rel, err)
			}
		}
	}
	have := map[string]bool{}
	for _, r := range rels {
		have[r] = true
	}
	removed := 0
	for _, r := range oldManifest {
		if have[r] || keep[strings.SplitN(r, "/", 2)[0]] {
			continue
		}
		removed++
		fmt.Fprintf(o.Out, "%s  %s\n", verb(o.DryRun, "remove"), r)
		if !o.DryRun {
			os.Remove(filepath.Join(target, filepath.FromSlash(r)))
		}
	}
	fmt.Fprintf(o.Out, "%d file(s) %s, %d obsolete %s.\n", changed, tern(o.DryRun, "to copy", "copied"), removed, tern(o.DryRun, "to remove", "removed"))
	if o.DryRun {
		return nil
	}
	os.WriteFile(filepath.Join(target, "MANIFEST"), []byte(strings.Join(rels, "\n")+"\n"), 0o644)
	if runtime.GOOS == "windows" {
		exec.Command("attrib", "+h", target).Run()
	}
	tidy(target)

	label := o.Label
	if label == "" {
		label = filepath.Base(disk)
		if label == "" || label == `\` || label == "/" {
			label = disk
		}
	}
	index := filepath.Join(target, "index.db")
	_, statErr := os.Stat(index)
	if statErr != nil && !o.NoScan && (o.Scan || o.Yes || ask(o, fmt.Sprintf("Scan the disk now as '%s'? (about a minute per 100,000 files)", label), true)) {
		fmt.Fprintln(o.Out, "Scanning...")
		host, _ := os.Hostname()
		st, err := scan.Run(scan.Options{Root: disk, DB: index, Label: label, Host: host, HashMin: -1, Text: true, Log: o.Err})
		if err != nil {
			return fmt.Errorf("the scan failed: %w", err)
		}
		fmt.Fprintln(o.Out, st.Summary(label))
	} else if statErr == nil {
		fmt.Fprintln(o.Out, "An index already exists here: it was kept. The app re-checks the disk when it starts.")
	} else {
		fmt.Fprintf(o.Out, "No index yet. Create it with:\n  \"%s\" scan \"%s\" --db \"%s\" --source \"%s\"\n", filepath.Join(target, "sidecar"), disk, index, label)
	}
	tidy(target)
	launcher := "open-sidecar.command"
	if runtime.GOOS == "windows" {
		launcher = "open-sidecar.bat"
	}
	fmt.Fprintf(o.Out, "\nDone. To open the catalog, double click:\n  %s\n", filepath.Join(target, launcher))
	if len(missingPlatforms(items)) > 0 {
		fmt.Fprintf(o.Out, "\nThis disk will only open on %s/%s for now. To use it on other systems, run the installer again from a\ndownload of those builds, or point --from at a folder that contains them (sidecar-<os>-<arch>).\n", runtime.GOOS, runtime.GOARCH)
	}
	return nil
}

func missingPlatforms(items []item) []string {
	n := 0
	for _, it := range items {
		if strings.HasPrefix(it.rel, "bin/") {
			n++
		}
	}
	if n >= 2 {
		return nil
	}
	return []string{"others"}
}

func verb(dry bool, v string) string {
	if dry {
		return "would " + v
	}
	return v
}

func tern(c bool, a, b string) string {
	if c {
		return a
	}
	return b
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// tidy removes the ._ files macOS leaves on exFAT/NTFS and any folder left empty.
func tidy(target string) {
	var dirs []string
	filepath.WalkDir(target, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			dirs = append(dirs, p)
		} else if strings.HasPrefix(d.Name(), "._") {
			os.Remove(p)
		}
		return nil
	})
	for i := len(dirs) - 1; i > 0; i-- { // never the .sidecar folder itself (index 0)
		if keep[filepath.Base(dirs[i])] {
			continue
		}
		os.Remove(dirs[i]) // only succeeds if empty
	}
}
