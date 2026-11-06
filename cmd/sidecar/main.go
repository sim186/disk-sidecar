// Sidecar: a catalog that lives on each of your external disks.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/sim186/disk-sidecar/internal/assets"
	"github.com/sim186/disk-sidecar/internal/hashdups"
	"github.com/sim186/disk-sidecar/internal/install"
	"github.com/sim186/disk-sidecar/internal/mcp"
	"github.com/sim186/disk-sidecar/internal/merge"
	"github.com/sim186/disk-sidecar/internal/phone"
	"github.com/sim186/disk-sidecar/internal/scan"
	"github.com/sim186/disk-sidecar/internal/server"
)

const usage = `Sidecar %s: a catalog that lives on each of your external disks.

  sidecar install <disk>      put Sidecar on a disk (or update it), and scan it
  sidecar serve               open the catalog in your browser (what a double click does)
  sidecar scan <folder>       index a folder or disk (incremental)
  sidecar phone [scan <serial>]  list connected phones, or catalogue one (Android via adb, iPhone photos via ifuse)
  sidecar hash                confirm duplicates with SHA-256
  sidecar merge <main> <other>  import another index
  sidecar mcp                 let an LLM search the index (MCP over stdio)
  sidecar version

Run "sidecar <command> -h" for the options of a command.
`

// stringList is a repeatable flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// parse lets the positional arguments come before, between or after the flags.
func parse(fs *flag.FlagSet, args []string) []string {
	boolFlags := map[string]bool{}
	fs.VisitAll(func(f *flag.Flag) {
		if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
			boolFlags[f.Name] = true
		}
	})
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			if !strings.Contains(name, "=") && !boolFlags[name] && name != "h" && name != "help" && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		} else {
			pos = append(pos, a)
		}
	}
	if err := fs.Parse(flags); err != nil {
		os.Exit(2)
	}
	return pos
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "sidecar:", err)
	os.Exit(1)
}

func main() {
	if len(os.Args) < 2 { // double click: open the app (it also offers to install on a disk)
		os.Args = append(os.Args, "serve")
	}
	cmd, args := os.Args[1], os.Args[2:]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	switch cmd {
	case "version", "--version", "-v":
		fmt.Println("disk-sidecar", assets.Version)

	case "help", "--help", "-h":
		fmt.Printf(usage, assets.Version)

	case "scan":
		db := fs.String("db", "", "index to write (default: <folder>/.sidecar/index.db)")
		source := fs.String("source", "", "name of this disk or computer in the app (default: the folder name)")
		hashMin := fs.Float64("hash-min-mb", -1, "also SHA-256 files of at least this size while scanning")
		noText := fs.Bool("no-text", false, "do not read file contents")
		withText := fs.Bool("text", false, "read file contents in your home folder too (by default it is scanned by name only)")
		pos := parse(fs, args)
		if len(pos) != 1 {
			die(fmt.Errorf("usage: sidecar scan <folder> [--db index.db] [--source name]"))
		}
		root, _ := filepath.Abs(pos[0])
		if *db == "" {
			*db = filepath.Join(root, ".sidecar", "index.db")
		}
		if *source == "" {
			*source = filepath.Base(root)
		}
		hm := int64(-1)
		if *hashMin >= 0 {
			hm = int64(*hashMin * 1024 * 1024)
		}
		host, _ := os.Hostname()
		st, err := scan.Run(scan.Options{Root: root, DB: *db, Label: *source, Host: host, HashMin: hm, Text: !*noText && (*withText || !scan.IsHome(root)), Log: os.Stderr})
		if err != nil {
			die(err)
		}
		fmt.Println(st.Summary(*source))

	case "phone":
		pos := parse(fs, args)
		if len(pos) == 2 && pos[0] == "scan" {
			d, ok := phone.Find(pos[1])
			if !ok {
				die(fmt.Errorf("no connected phone with serial %q (see: sidecar phone)", pos[1]))
			}
			st, err := phone.Scan(d, os.Stderr, nil)
			if err != nil {
				die(err)
			}
			fmt.Println(st.Summary(d.Name))
			break
		}
		if len(pos) != 0 {
			die(fmt.Errorf("usage: sidecar phone [scan <serial>]"))
		}
		ds := phone.Detect()
		if len(ds) == 0 {
			fmt.Println("no phone found (Android needs adb and USB debugging; iPhone needs libimobiledevice and ifuse)")
		}
		for _, d := range ds {
			fmt.Printf("%-8s %-24s %-18s %s %s\n", d.Platform, d.Serial, d.State, d.Name, d.Hint)
		}

	case "hash":
		db := fs.String("db", "", "index to update (default: the index of the disk this program is on)")
		minMB := fs.Float64("min-mb", 1, "ignore files smaller than this")
		also := fs.String("also-match", "", "another index whose file sizes count as ties (finds copies across disks)")
		var roots stringList
		fs.Var(&roots, "root", "LABEL=folder: where a disk is mounted on this computer (repeatable)")
		parse(fs, args)
		if *db == "" {
			*db = mcp.DefaultDB()
		}
		if *db == "" {
			die(fmt.Errorf("no index: use --db"))
		}
		over := map[string]string{}
		for _, r := range roots {
			if k, v, ok := strings.Cut(r, "="); ok {
				over[k] = v
			}
		}
		res, err := hashdups.Run(hashdups.Options{DB: *db, MinMB: *minMB, AlsoMatch: *also, Roots: over, Log: os.Stderr})
		if err != nil {
			die(err)
		}
		fmt.Println(res.Summary())

	case "merge":
		var labels, renames stringList
		fs.Var(&labels, "label", "only import this source (repeatable)")
		fs.Var(&renames, "rename", "new name for the source selected by --label (same order)")
		dry := fs.Bool("dry-run", false, "show what would happen, change nothing")
		noBak := fs.Bool("no-backup", false, "do not write index.db.bak first")
		pos := parse(fs, args)
		if len(pos) != 2 {
			die(fmt.Errorf("usage: sidecar merge <main index> <other index>"))
		}
		rn := map[string]string{}
		for i, l := range labels {
			if i < len(renames) {
				rn[l] = renames[i]
			}
		}
		if err := merge.Run(merge.Options{Main: pos[0], Other: pos[1], Labels: labels, Rename: rn, DryRun: *dry, NoBackup: *noBak, Log: os.Stdout}); err != nil {
			die(err)
		}

	case "serve":
		serveFromHomeDisk(args) // passes the baton to a copy on this computer; never returns when it works
		cfg := server.Config{}
		fs.IntVar(&cfg.Port, "port", 8765, "port on 127.0.0.1")
		fs.StringVar(&cfg.HomeDB, "db", "", "use this index as the home disk (development)")
		fs.StringVar(&cfg.HomeDisk, "home-disk", "", "the disk this program is installed on (set when it re-executes from a copy on this computer)")
		var mounts stringList
		fs.Var(&mounts, "mounts", "extra folder that contains mounted disks (repeatable)")
		fs.BoolVar(&cfg.NoOpen, "no-open", false, "do not open the browser")
		fs.BoolVar(&cfg.NoVerify, "no-verify", false, "do not re-check the disks at start")
		fs.BoolVar(&cfg.NoUpdateCheck, "no-update-check", false, "do not ask GitHub whether a newer release exists (or set SIDECAR_NO_UPDATE_CHECK=1)")
		fs.BoolVar(&cfg.NoWatch, "no-watch", false, "do not offer to set up disks plugged in while running")
		fs.BoolVar(&cfg.NoDefaultMounts, "no-default-mounts", false, "do not look in /Volumes, /media, /mnt (testing)")
		fs.DurationVar(&cfg.IdleExit, "idle-exit", 30*time.Minute, "stop once no page has been open for this long (0 = never)")
		parse(fs, args)
		cfg.Mounts = mounts
		if err := server.Run(cfg, os.Stdout); err != nil {
			die(err)
		}

	case "mcp":
		db := fs.String("db", "", "index to read (default: SIDECAR_DB, or the disk this program is on)")
		parse(fs, args)
		if *db == "" {
			*db = mcp.DefaultDB()
		}
		if err := mcp.Serve(*db, os.Stdin, os.Stdout); err != nil {
			die(err)
		}

	case "install":
		label := fs.String("label", "", "name of the disk shown in the app (default: the folder name)")
		doScan := fs.Bool("scan", false, "scan the disk after installing, without asking")
		noScan := fs.Bool("no-scan", false, "do not offer to scan")
		yes := fs.Bool("yes", false, "do not ask questions")
		dry := fs.Bool("dry-run", false, "show what would be done")
		var from stringList
		fs.Var(&from, "from", "folder with builds for other systems, to put on the disk too (repeatable)")
		pos := parse(fs, args)
		if len(pos) != 1 {
			die(fmt.Errorf("usage: sidecar install <disk>   (for example /Volumes/MyDisk or E:\\)"))
		}
		err := install.Run(install.Options{Disk: pos[0], Label: *label, Scan: *doScan, NoScan: *noScan, Yes: *yes, DryRun: *dry,
			From: from, Out: os.Stdout, Err: os.Stderr, In: os.Stdin})
		if err != nil {
			die(err)
		}

	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		fmt.Fprintf(os.Stderr, usage, assets.Version)
		os.Exit(2)
	}
}

// serveFromHomeDisk keeps the server from running with its code on a disk that can be ejected.
// macOS unmounts external disks on sleep and on a forced unplug; a program that still runs on
// one starts firing on unreadable code pages (it catches every CPU, and even Ctrl+C no longer
// works). So when the program runs off a mounted disk, it copies itself once to this computer's
// own disk and goes on from that copy: the disk may then be ejected or unplugged at any time;
// the server inside stays healthy, keeps its port and ends cleanly on --idle-exit.
func serveFromHomeDisk(args []string) {
	if os.Getenv("SIDECAR_RUN_IN_PLACE") != "" {
		return // we are the copy on this computer already
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	root, onDisk := diskMount(exe)
	if !onDisk {
		return
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		noteRunningOnDisk(root, err)
		return
	}
	f, err := os.Open(exe)
	if err != nil {
		noteRunningOnDisk(root, err)
		return
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		f.Close()
		noteRunningOnDisk(root, err)
		return
	}
	name := hex.EncodeToString(h.Sum(nil))[:8]
	f.Close()

	place := filepath.Join(dir, "disk-sidecar", "server", name)
	if os.MkdirAll(place, 0o755) != nil {
		noteRunningOnDisk(root, os.ErrPermission)
		return
	}
	os.Remove(filepath.Join(place, "copy.part"))
	tmp := filepath.Join(place, "copy.part")
	if copySelf(exe, tmp) != nil {
		noteRunningOnDisk(root, fmt.Errorf("the copy to %s failed", place))
		return
	}
	if os.Chmod(tmp, 0o755) != nil {
		noteRunningOnDisk(root, nil)
		return
	}
	newExe := filepath.Join(place, filepath.Base(exe))
	os.Remove(newExe)
	if os.Rename(tmp, newExe) != nil {
		noteRunningOnDisk(root, nil)
		return
	}
	if runtime.GOOS == "windows" { // exec replaces the process in os on windows
		noteRunningOnDisk(root, nil)
		return
	}
	argv := append([]string{}, os.Args...)
	if !passed(args, "db") { // keep which disk this server belongs to across the copy
		argv = append(argv, "-home-disk", root)
	}
	env := append(os.Environ(), "SIDECAR_RUN_IN_PLACE=1")
	fmt.Fprintf(os.Stderr, "Running from %s so %s can be ejected or unplugged at any time.\n", place, root)
	if err := syscall.Exec(newExe, argv, env); err != nil {
		die(fmt.Errorf("cannot start the copy on %s: %w", place, err))
	}
}

// diskMount says whether a path lives on a mounted disk: what root folder (darwin /Volumes/<name>,
// linux /media/..., /run/media/..., /mnt/...) and with it "yes". This computer's own disk is no.
func diskMount(p string) (root string, yes bool) {
	switch runtime.GOOS {
	case "darwin":
		if rest, ok := strings.CutPrefix(p, "/Volumes/"); ok {
			if name, _, _ := strings.Cut(rest, "/"); name != "" {
				if real, err := filepath.EvalSymlinks("/Volumes/" + name); err == nil && real == "/" {
					return "", false // the system disk itself ("Macintosh HD" is a copy of it)
				}
				return "/Volumes/" + name, true
			}
		}
	case "linux", "freebsd":
		for _, base := range []string{"/media/", "/run/media/", "/mnt/"} {
			if rest, ok := strings.CutPrefix(p, base); ok {
				if name, _, _ := strings.Cut(rest, "/"); name != "" {
					return base + strings.SplitN(rest, "/", 2)[0], true
				}
			}
		}
	}
	return "", false
}

// copySelf copies the program to to, so a copy can run on this computer.
func copySelf(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(to)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(to)
		return err
	}
	return nil
}

// passed says args carry a flag ( -flag or --flag, both with = or a separate value).
func passed(args []string, flag string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "-") && strings.TrimLeft(a, "-") == flag ||
			strings.HasPrefix(a, "-") && strings.HasPrefix(strings.TrimLeft(a, "-"), flag+"=") {
			return true
		}
	}
	return false
}

// noteRunningOnDisk says the program stays on its disk (the copy did not make it) and why.
func noteRunningOnDisk(root string, why error) {
	if why != nil {
		fmt.Fprintf(os.Stderr, "Staying on %s (%s). It should not be ejected while this server runs.\n", root, why)
		return
	}
	fmt.Fprintf(os.Stderr, "Staying on %s. It should not be ejected while this server runs.\n", root)
}
