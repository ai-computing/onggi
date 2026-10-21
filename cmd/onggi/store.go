package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ai-computing/onggi/pkg/backend"
	bfs "github.com/ai-computing/onggi/pkg/backend/fs"
	bs3 "github.com/ai-computing/onggi/pkg/backend/s3"
	"github.com/ai-computing/onggi/pkg/chunker"
	"github.com/ai-computing/onggi/pkg/store"
)

type storeFlags struct {
	url, durability, min, avg, max, packSize *string
}

func addStoreFlags(fs *flag.FlagSet) storeFlags {
	return storeFlags{
		url:        fs.String("store", os.Getenv("ONGGI_STORE"), "store: a directory, file:// URL or s3://bucket/prefix?endpoint=host:port URL (env ONGGI_STORE)"),
		durability: fs.String("durability", "unknown", "what the storage guarantees: none, replicated, erasure-coded, unknown"),
		min:        fs.String("min", "16KiB", "minimum chunk size (only when creating a store)"),
		avg:        fs.String("avg", "64KiB", "average chunk size (only when creating a store)"),
		max:        fs.String("max", "256KiB", "maximum chunk size (only when creating a store)"),
		packSize:   fs.String("pack-size", "64MiB", "pack object size (only when creating a store)"),
	}
}

// backendRefs is what a store driver provides: objects and refs.
type backendRefs interface {
	backend.Backend
	backend.Refs
}

// open returns the store and its refs, picking the driver from the URL scheme (ADR-0008).
func (f storeFlags) open(ctx context.Context) (*store.Store, backend.Refs, error) {
	u := *f.url
	if u == "" {
		return nil, nil, errors.New("no store: pass --store or set ONGGI_STORE")
	}
	var b backendRefs
	var err error
	d := backend.Durability(*f.durability)
	switch {
	case strings.HasPrefix(u, "s3://"):
		b, err = bs3.Open(u, d)
	case strings.Contains(u, "://") && !strings.HasPrefix(u, "file://"):
		err = fmt.Errorf("store %q: want a directory, file:// or s3:// URL", u)
	default:
		b, err = bfs.Open(strings.TrimPrefix(u, "file://"), d)
	}
	if err != nil {
		return nil, nil, err
	}
	var p chunker.Params
	if p.Min, err = size(*f.min); err == nil {
		if p.Avg, err = size(*f.avg); err == nil {
			p.Max, err = size(*f.max)
		}
	}
	if err != nil {
		return nil, nil, err
	}
	pack, err := size(*f.packSize)
	if err != nil {
		return nil, nil, err
	}
	s, err := store.Open(ctx, b, p, int64(pack))
	return s, b, err
}

func push(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("push", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sf := addStoreFlags(fs)
	ref := fs.String("ref", "", "set this ref to the new manifest")
	workers := fs.Int("workers", runtime.NumCPU(), "files chunked at once")
	uploads := fs.Int("uploads", 4, "packs uploaded at once")
	tmp := fs.String("tmp", "", "directory for packs being assembled (default: system temp)")
	asJSON := fs.Bool("json", false, "print JSON")
	quiet := fs.Bool("quiet", false, "no progress on stderr")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: onggi push --store S [--ref NAME] <dir>")
		return 2
	}
	ctx := context.Background()
	s, refs, err := sf.open(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "onggi:", err)
		return 1
	}
	o := store.PushOptions{Workers: *workers, Uploads: *uploads, TempDir: *tmp}
	start := time.Now()
	if !*quiet {
		o.Progress = func(st store.PushStats) {
			el := time.Since(start).Seconds()
			fmt.Fprintf(stderr, "\r[%d/%d] %s / %s read, %s new  %.0f MB/s ", st.Files, st.FilesTotal, human(st.Bytes),
				human(st.BytesTotal), human(st.NewBytes), float64(st.Bytes)/1e6/max(el, 1e-9))
		}
	}
	md, st, err := s.Push(ctx, fs.Arg(0), o)
	if !*quiet {
		fmt.Fprintln(stderr)
	}
	if err != nil {
		fmt.Fprintln(stderr, "onggi:", err)
		return 1
	}
	if *ref != "" {
		old, gerr := refs.GetRef(ctx, *ref)
		if gerr != nil && !errors.Is(gerr, backend.ErrNotFound) {
			fmt.Fprintln(stderr, "onggi:", gerr)
			return 1
		}
		if err := refs.SetRef(ctx, *ref, old, md.String()); err != nil {
			fmt.Fprintln(stderr, "onggi:", err)
			return 1
		}
	}
	if *asJSON {
		return writeJSON(stdout, stderr, map[string]any{"manifest": md.String(), "ref": *ref, "stats": st})
	}
	fmt.Fprintf(stdout, "manifest %s\n", md)
	if *ref != "" {
		fmt.Fprintf(stdout, "ref      %s\n", *ref)
	}
	fmt.Fprintf(stdout, "files    %d (%d skipped)\nread     %s\nnew      %s in %d packs\ntook     %s (%.0f MB/s)\n",
		st.Files, len(st.Skipped), human(st.Bytes), human(st.NewBytes), st.Packs, st.Duration.Round(time.Second),
		float64(st.Bytes)/1e6/max(st.Duration.Seconds(), 1e-9))
	return 0
}

func pull(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pull", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sf := addStoreFlags(fs)
	workers := fs.Int("workers", 8, "ranges read at once")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil || fs.NArg() != 2 {
		fmt.Fprintln(stderr, "usage: onggi pull --store S <ref|sha256:...> <dir>")
		return 2
	}
	ctx := context.Background()
	s, refs, err := sf.open(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "onggi:", err)
		return 1
	}
	md, err := store.Resolve(ctx, refs, fs.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, "onggi:", err)
		return 1
	}
	st, err := s.Pull(ctx, md, fs.Arg(1), store.PullOptions{Workers: *workers})
	if err != nil {
		fmt.Fprintln(stderr, "onggi:", err)
		return 1
	}
	if *asJSON {
		return writeJSON(stdout, stderr, map[string]any{"manifest": md.String(), "stats": st})
	}
	fmt.Fprintf(stdout, "manifest %s\nfiles    %d\nwritten  %s (read %s in %d ranges)\ntook     %s (%.0f MB/s)\n",
		md, st.Files, human(st.Bytes), human(st.ReadBytes), st.Ranges, st.Duration.Round(time.Second),
		float64(st.Bytes)/1e6/max(st.Duration.Seconds(), 1e-9))
	return 0
}

func ls(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sf := addStoreFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx := context.Background()
	s, refs, err := sf.open(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "onggi:", err)
		return 1
	}
	type row struct{ name, value string }
	var rows []row
	if err := refs.ListRefs(ctx, func(n, v string) error { rows = append(rows, row{n, v}); return nil }); err != nil {
		fmt.Fprintln(stderr, "onggi:", err)
		return 1
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "REF\tMANIFEST\tBYTES")
	for _, r := range rows {
		size := "?"
		if md, err := store.Resolve(ctx, nil, r.value); err == nil {
			if m, err := s.ReadManifest(ctx, md); err == nil {
				size = human(m.Bytes)
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", r.name, r.value, size)
	}
	tw.Flush()
	var packBytes int64
	var packs int
	if err := s.B.List(ctx, backend.Pack, func(_ string, n int64) error { packs++; packBytes += n; return nil }); err != nil {
		fmt.Fprintln(stderr, "onggi:", err)
		return 1
	}
	chunks, _ := s.Stats()
	c := s.Config.Chunker
	fmt.Fprintf(stdout, "\nstore: %d chunks in %d packs, %s; chunker %s/%s/%s; durability %s\n", chunks, packs, human(packBytes),
		human(int64(c.Min)), human(int64(c.Avg)), human(int64(c.Max)), s.B.Caps().Durability)
	return 0
}

func inspect(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sf := addStoreFlags(fs)
	asJSON := fs.Bool("json", false, "print the manifest as JSON")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: onggi inspect --store S <ref|sha256:...>")
		return 2
	}
	ctx := context.Background()
	s, refs, err := sf.open(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "onggi:", err)
		return 1
	}
	md, err := store.Resolve(ctx, refs, fs.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, "onggi:", err)
		return 1
	}
	m, err := s.ReadManifest(ctx, md)
	if err != nil {
		fmt.Fprintln(stderr, "onggi:", err)
		return 1
	}
	if *asJSON {
		return writeJSON(stdout, stderr, m)
	}
	fmt.Fprintf(stdout, "manifest %s\ncreated  %s\nbytes    %s in %d files\n\n", md, m.Created.Format(time.RFC3339), human(m.Bytes), len(m.Files))
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KIND\tSIZE\tPATH")
	for _, f := range m.Files {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", f.Kind, human(f.Size), f.Path)
	}
	tw.Flush()
	return 0
}

func writeJSON(stdout, stderr io.Writer, v any) int {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintln(stderr, "onggi:", err)
		return 1
	}
	return 0
}
