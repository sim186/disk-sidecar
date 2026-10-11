// Sidecar: a catalog that lives on each of your external disks.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sim186/disk-sidecar/internal/assets"
	"github.com/sim186/disk-sidecar/internal/hashdups"
	"github.com/sim186/disk-sidecar/internal/install"
	"github.com/sim186/disk-sidecar/internal/mcp"
	"github.com/sim186/disk-sidecar/internal/merge"
	"github.com/sim186/disk-sidecar/internal/scan"
	"github.com/sim186/disk-sidecar/internal/server"
)

const usage = `Sidecar %s: a catalog that lives on each of your external disks.

  sidecar install <disk>      put Sidecar on a disk (or update it), and scan it
  sidecar serve               open the catalog in your browser
  sidecar scan <folder>       index a folder or disk (incremental)
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
	if len(os.Args) < 2 {
		fmt.Printf(usage, assets.Version)
		return
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
		st, err := scan.Run(scan.Options{Root: root, DB: *db, Label: *source, Host: host, HashMin: hm, Text: !*noText, Log: os.Stderr})
		if err != nil {
			die(err)
		}
		fmt.Println(st.Summary(*source))

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
		cfg := server.Config{}
		fs.IntVar(&cfg.Port, "port", 8765, "port on 127.0.0.1")
		fs.StringVar(&cfg.HomeDB, "db", "", "use this index as the home disk (development)")
		var mounts stringList
		fs.Var(&mounts, "mounts", "extra folder that contains mounted disks (repeatable)")
		fs.BoolVar(&cfg.NoOpen, "no-open", false, "do not open the browser")
		fs.BoolVar(&cfg.NoVerify, "no-verify", false, "do not re-check the disks at start")
		fs.BoolVar(&cfg.NoDefaultMounts, "no-default-mounts", false, "do not look in /Volumes, /media, /mnt (testing)")
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
