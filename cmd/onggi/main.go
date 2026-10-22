// Command onggi is the onggi CLI.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ai-computing/onggi/pkg/dedup"
)

var version = "dev"

const usage = `onggi — content-addressed store for model weights and compiled artifacts

usage:
  onggi push    --store S [--ref NAME] <dir>     store a directory; only missing chunks are uploaded
  onggi pull    --store S <ref|sha256:...> <dir> restore an artifact, every chunk hash-checked
  onggi ls      --store S                        refs and store totals
  onggi inspect --store S <ref|sha256:...>       files of an artifact
  onggi rm-ref  --store S NAME                   delete a ref (its artifact becomes garbage)
  onggi gc      --store S [--grace 24h] [--dry-run]
                                                 mark and sweep: a run records candidates, a run
                                                 after the grace period deletes them
  onggi gc      --store CACHE --roots-store S [--keep-used 168h]
                                                 evict from a node cache what recently used
                                                 artifacts do not need
  onggi dedup-report [flags] <path>...           measure how much the files would shrink in onggi
  onggi csi     --store S [flags]                CSI node plugin: serve artifacts as read-only volumes (Linux)
  onggi version

A store S is a directory or file:// URL on any POSIX mount (local disk, NFS, SMB, CephFS, ...).
S may also be s3://bucket/prefix?endpoint=host:port[&insecure=1][&region=r]; credentials come from
AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY or the usual AWS files.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "dedup-report":
		os.Exit(dedupReport(os.Args[2:], os.Stdout, os.Stderr))
	case "push":
		os.Exit(push(os.Args[2:], os.Stdout, os.Stderr))
	case "pull":
		os.Exit(pull(os.Args[2:], os.Stdout, os.Stderr))
	case "ls":
		os.Exit(ls(os.Args[2:], os.Stdout, os.Stderr))
	case "inspect":
		os.Exit(inspect(os.Args[2:], os.Stdout, os.Stderr))
	case "csi":
		os.Exit(csiNode(os.Args[2:], os.Stderr))
	case "rm-ref":
		os.Exit(rmRef(os.Args[2:], os.Stdout, os.Stderr))
	case "gc":
		os.Exit(gc(os.Args[2:], os.Stdout, os.Stderr))
	case "version":
		fmt.Println(version)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}

func dedupReport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dedup-report", flag.ContinueOnError)
	fs.SetOutput(stderr)
	minC := fs.String("min", "16KiB", "minimum chunk size")
	avgC := fs.String("avg", "64KiB", "average chunk size (power of two)")
	maxC := fs.String("max", "256KiB", "maximum chunk size")
	workers := fs.Int("workers", runtime.NumCPU(), "files chunked at once")
	minSize := fs.String("min-file", "0", "skip files smaller than this")
	pairs := fs.Int("pairs", 0, "pairwise shares among the N largest files")
	fixed := fs.String("fixed-block", "", "also measure fixed-size blocks of this size (e.g. 1MiB)")
	asJSON := fs.Bool("json", false, "print JSON instead of tables")
	quiet := fs.Bool("quiet", false, "no progress on stderr")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: onggi dedup-report [flags] <path>...")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return 2
	}
	var o dedup.Options
	var err error
	o.Workers, o.Pairs = *workers, *pairs
	if o.Params.Min, err = size(*minC); err == nil {
		if o.Params.Avg, err = size(*avgC); err == nil {
			o.Params.Max, err = size(*maxC)
		}
	}
	var min int
	if err == nil {
		min, err = size(*minSize)
		o.MinSize = int64(min)
	}
	if err == nil && *fixed != "" {
		o.FixedBlock, err = size(*fixed)
	}
	if err == nil {
		err = o.Params.Validate()
	}
	if err != nil {
		fmt.Fprintln(stderr, "onggi:", err)
		return 2
	}

	files, skipped, err := dedup.Collect(fs.Args(), o.MinSize)
	if err != nil {
		fmt.Fprintln(stderr, "onggi:", err)
		return 1
	}
	var total int64
	for _, f := range files {
		total += f.Bytes
	}
	start := time.Now()
	var doneBytes int64
	progress := func(done, n int, f dedup.File) {
		doneBytes += f.Bytes
		if !*quiet {
			el := time.Since(start).Seconds()
			fmt.Fprintf(stderr, "\r[%d/%d] %s / %s  %.0f MB/s ", done, n, human(doneBytes), human(total), float64(doneBytes)/1e6/max(el, 1e-9))
		}
	}
	rep, err := dedup.Run(files, skipped, o, progress)
	if !*quiet {
		fmt.Fprintln(stderr)
	}
	if err != nil {
		fmt.Fprintln(stderr, "onggi:", err)
		return 1
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintln(stderr, "onggi:", err)
			return 1
		}
		return 0
	}
	printReport(stdout, rep, time.Since(start))
	return 0
}

func printReport(w io.Writer, r *dedup.Report, took time.Duration) {
	fmt.Fprintf(w, "chunks: content-defined, min %s / avg %s / max %s\n\n", human(int64(r.Params.Min)), human(int64(r.Params.Avg)), human(int64(r.Params.Max)))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "kind\tfiles\tbytes\tunique in kind\tnew\t")
	for _, g := range r.Groups {
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t\n", g.Kind, g.Files, human(g.Bytes), human(g.Unique), human(g.NewBytes))
	}
	fmt.Fprintf(tw, "total\t%d\t%s\t\t%s\t\n", len(r.Files), human(r.Bytes), human(r.UniqueBytes))
	tw.Flush()
	fmt.Fprintf(w, "\nstored after dedup: %s of %s (%.2fx)\n", human(r.UniqueBytes), human(r.Bytes), r.Ratio)
	if r.Fixed != nil {
		fmt.Fprintf(w, "fixed %s blocks:   %s (%.2fx)\n", human(int64(r.Fixed.Block)), human(r.Fixed.UniqueBytes), r.Fixed.Ratio)
	}
	if len(r.Pairs) > 0 {
		fmt.Fprintln(w, "\nshare of A's bytes found in B:")
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, p := range r.Pairs {
			fmt.Fprintf(tw, "  %s\t→ %s\t%5.1f %%\n", filepath.Base(p.A), filepath.Base(p.B), 100*p.Share)
		}
		tw.Flush()
	}
	if len(r.Skipped) > 0 {
		fmt.Fprintf(w, "\nskipped %d symlinks or unreadable entries\n", len(r.Skipped))
	}
	fmt.Fprintf(w, "took %s\n", took.Round(time.Second))
}

// size parses 64KiB, 1MiB, 2GiB, 1000 (bytes).
func size(s string) (int, error) {
	s = strings.TrimSpace(s)
	mult := 1
	for _, u := range []struct {
		suffix string
		m      int
	}{{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30}} {
		if strings.HasSuffix(s, u.suffix) {
			s, mult = strings.TrimSuffix(s, u.suffix), u.m
			break
		}
	}
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n < 0 {
		return 0, fmt.Errorf("bad size %q", s)
	}
	return n * mult, nil
}

func human(b int64) string {
	const unit = 1000
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "kMGTPE"[exp])
}
