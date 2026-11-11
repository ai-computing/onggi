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
	"github.com/ai-computing/onggi/pkg/codec"
	"github.com/ai-computing/onggi/pkg/digest"
	"github.com/ai-computing/onggi/pkg/layout"
	"github.com/ai-computing/onggi/pkg/store"
)

type storeFlags struct {
	url, durability, min, avg, max, sub, subKinds, packSize, frame *string
}

func addStoreFlags(fs *flag.FlagSet) storeFlags {
	return storeFlags{
		url:        fs.String("store", os.Getenv("ONGGI_STORE"), "store: a directory, file:// URL or s3://bucket/prefix?endpoint=host:port URL (env ONGGI_STORE)"),
		durability: fs.String("durability", "unknown", "what the storage guarantees: none, replicated, erasure-coded, unknown"),
		min:        fs.String("min", "16KiB", "minimum chunk size (only when creating a store)"),
		avg:        fs.String("avg", "64KiB", "average chunk size (only when creating a store)"),
		max:        fs.String("max", "256KiB", "maximum chunk size (only when creating a store)"),
		sub:        fs.String("sub", "4KiB", "average sub-chunk size for two-level chunking, 0 for one level (only when creating a store)"),
		subKinds:   fs.String("sub-kinds", "atom-prefill,atom-decode,atom-other", "file kinds that get sub-chunks, or all (only when creating a store)"),
		packSize:   fs.String("pack-size", "64MiB", "pack object size (only when creating a store)"),
		frame:      fs.String("frame", "256KiB", "compress new chunks of sub-chunked files together up to this size, 0 for one by one (only when creating a store)"),
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
	c := store.Create{Chunker: p, PackSize: int64(pack)}
	if *f.sub != "0" {
		avg, err := size(*f.sub)
		if err != nil {
			return nil, nil, err
		}
		c.Sub = &chunker.Params{Min: avg / 4, Avg: avg, Max: avg * 4}
		if *f.frame != "0" {
			fr, err := size(*f.frame)
			if err != nil {
				return nil, nil, err
			}
			c.Frame = int64(fr)
		}
		if *f.subKinds != "all" {
			for _, k := range strings.Split(*f.subKinds, ",") {
				c.SubKinds = append(c.SubKinds, layout.Kind(strings.TrimSpace(k)))
			}
		}
	}
	s, err := store.OpenWith(ctx, b, c)
	return s, b, err
}

func push(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("push", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sf := addStoreFlags(fs)
	ref := fs.String("ref", "", "set this ref to the new manifest")
	profileFile := fs.String("profile", "", "exact compatibility profile JSON to bind to the artifact")
	workers := fs.Int("workers", runtime.NumCPU(), "files chunked at once")
	uploads := fs.Int("uploads", 4, "packs uploaded at once")
	compress := fs.String("compress", "auto", "how new chunks are stored: auto (zstd or byte planes + Huffman, whichever is smaller), zstd, none")
	tmp := fs.String("tmp", "", "directory for packs being assembled (default: system temp)")
	asJSON := fs.Bool("json", false, "print JSON")
	quiet := fs.Bool("quiet", false, "no progress on stderr")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: onggi push --store S [--ref NAME] <dir>")
		return 2
	}
	mode, err := codec.ParseMode(*compress)
	if err != nil {
		fmt.Fprintln(stderr, "onggi:", err)
		return 2
	}
	var profile *store.Profile
	if *profileFile != "" {
		p, err := readProfileFile(*profileFile)
		if err != nil {
			fmt.Fprintln(stderr, "onggi:", err)
			return 2
		}
		profile = &p
	}
	ctx := context.Background()
	s, refs, err := sf.open(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "onggi:", err)
		return 1
	}
	o := store.PushOptions{Workers: *workers, Uploads: *uploads, TempDir: *tmp, Compress: mode, Profile: profile}
	start := time.Now()
	if !*quiet {
		o.Progress = func(st store.PushStats) {
			el := time.Since(start).Seconds()
			fmt.Fprintf(stderr, "\r[%d/%d] %s / %s read, %s new  %.0f MB/s ", st.Files, st.FilesTotal, human(st.Bytes),
				human(st.BytesTotal), human(st.NewBytes), float64(st.Bytes)/1e6/max(el, 1e-9))
		}
	}
	// the lease covers the push and the ref update: GC must not sweep between the two
	release, err := s.Lease(ctx, "push "+*ref)
	if err != nil {
		fmt.Fprintln(stderr, "onggi:", err)
		return 1
	}
	defer release()
	stopMem := startMemProfile()
	md, st, err := s.Push(ctx, fs.Arg(0), o)
	if peak := stopMem(); peak > 0 {
		fmt.Fprintln(stderr, "peak heap in use", human(int64(peak)))
	}
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
	fmt.Fprintf(stdout, "files    %d (%d skipped)\nread     %s\nnew      %s, stored as %s in %d packs\ntook     %s (%.0f MB/s)\n",
		st.Files, len(st.Skipped), human(st.Bytes), human(st.NewBytes), human(st.PackBytes), st.Packs, st.Duration.Round(time.Second),
		float64(st.Bytes)/1e6/max(st.Duration.Seconds(), 1e-9))
	return 0
}

func pull(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pull", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sf := addStoreFlags(fs)
	workers := fs.Int("workers", 8, "ranges read at once")
	cacheDir := fs.String("cache", "", "node chunk cache: a local directory; chunks it holds are not read from the store, chunks read from the store are added")
	peers := fs.String("peers", "", "comma-separated peer URLs (onggi peer serve) asked for chunks before the store")
	baseRef := fs.String("base", "", "an artifact already restored in --base-dir that the pull may turn into this one, writing only what differs; its files are moved away (#41)")
	baseDir := fs.String("base-dir", "", "the directory holding --base")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil || fs.NArg() != 2 {
		fmt.Fprintln(stderr, "usage: onggi pull --store S [--cache DIR] [--peers URLS] [--base REF --base-dir DIR] <ref|sha256:...> <dir>")
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
	o := store.PullOptions{Workers: *workers, Peers: peerClients(*peers)}
	if (*baseRef == "") != (*baseDir == "") {
		fmt.Fprintln(stderr, "onggi: --base and --base-dir go together")
		return 2
	}
	if *baseRef != "" {
		bd, err := store.Resolve(ctx, refs, *baseRef)
		if err != nil {
			fmt.Fprintln(stderr, "onggi: base:", err)
			return 1
		}
		o.Bases = []store.Base{{Manifest: bd, Dir: *baseDir}}
		o.Claim = func(b store.Base) (string, bool) { return b.Dir, true }
	}
	if *cacheDir != "" {
		cb, err := bfs.Open(*cacheDir, backend.DurabilityNone)
		if err == nil {
			o.Cache, err = store.OpenWith(ctx, cb, store.Create{Chunker: s.Config.Chunker, Sub: s.Config.Sub,
				SubKinds: s.Config.SubKinds, PackSize: s.Config.PackSize})
		}
		if err != nil {
			fmt.Fprintln(stderr, "onggi: node cache:", err)
			return 1
		}
	}
	stopMem := startMemProfile()
	st, err := s.Pull(ctx, md, fs.Arg(1), o)
	if peak := stopMem(); peak > 0 {
		fmt.Fprintln(stderr, "peak heap in use", human(int64(peak)))
	}
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
	if o.Cache != nil {
		fmt.Fprintln(stdout, "cache    read", human(st.CacheBytes), "from the node cache, added", human(st.Cached), "to it")
	}
	if len(o.Peers) > 0 {
		fmt.Fprintln(stdout, "peers    received", human(st.PeerBytes), "in", st.PeerChunks, "chunks")
	}
	if st.Base != "" {
		fmt.Fprintln(stdout, "base     took over", st.Base, "and left", human(st.Reused), "in place")
	} else if st.LocalBase != "" {
		fmt.Fprintln(stdout, "base     read", human(st.LocalBytes), "from", st.LocalBase, "where it is (its bytes sit at other offsets)")
	} else if len(o.Bases) > 0 {
		fmt.Fprintln(stdout, "base     not used: it holds too little of this artifact")
	}
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
	if err := s.LoadIndex(ctx); err != nil {
		fmt.Fprintln(stderr, "onggi:", err)
		return 1
	}
	chunks, _ := s.Stats()
	c := s.Config.Chunker
	fmt.Fprintf(stdout, "\nstore: %d chunks in %d packs, %s; chunker %s/%s/%s; durability %s\n", chunks, packs, human(packBytes),
		human(int64(c.Min)), human(int64(c.Avg)), human(int64(c.Max)), s.B.Caps().Durability)
	if sc := s.Config.Sub; sc != nil {
		kinds := "all kinds"
		if len(s.Config.SubKinds) > 0 {
			kinds = fmt.Sprint(s.Config.SubKinds)
		}
		fmt.Fprintf(stdout, "two-level: %d chunks held as sub-chunks of %s/%s/%s (%s)\n", s.Composites(),
			human(int64(sc.Min)), human(int64(sc.Avg)), human(int64(sc.Max)), kinds)
	}
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

// diff reports what turning a node's copy of base into ref takes (#38).
func diff(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sf := addStoreFlags(fs)
	asJSON := fs.Bool("json", false, "print the diff as JSON")
	ranges := fs.Bool("ranges", false, "list the changed byte ranges of each file (with --json: [offset, length] pairs)")
	if err := fs.Parse(args); err != nil || fs.NArg() != 2 {
		fmt.Fprintln(stderr, "usage: onggi diff --store S [--json] [--ranges] <base ref|sha256:...> <ref|sha256:...>")
		return 2
	}
	ctx := context.Background()
	s, refs, err := sf.open(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "onggi:", err)
		return 1
	}
	var ds [2]digest.Digest
	for i := range ds {
		if ds[i], err = store.Resolve(ctx, refs, fs.Arg(i)); err != nil {
			fmt.Fprintln(stderr, "onggi:", err)
			return 1
		}
	}
	st, err := s.Diff(ctx, ds[0], ds[1], store.DiffOptions{Ranges: *ranges})
	if err != nil {
		fmt.Fprintln(stderr, "onggi:", err)
		return 1
	}
	if *asJSON {
		return writeJSON(stdout, stderr, st)
	}
	pct := func(n int64) string {
		if st.Size == 0 {
			return "-"
		}
		return fmt.Sprintf("%.2f %%", 100*float64(n)/float64(st.Size))
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "size\t%s\t\n", human(st.Size))
	fmt.Fprintf(tw, "same offset\t%s\t%s\n", human(st.Same), pct(st.Same))
	fmt.Fprintf(tw, "  in whole %d B blocks\t%s\t%s\n", store.BlockSize, human(st.SameBlocks), pct(st.SameBlocks))
	fmt.Fprintf(tw, "elsewhere in base\t%s\t%s\n", human(st.Moved), pct(st.Moved))
	fmt.Fprintf(tw, "new\t%s\t%s\n", human(st.New), pct(st.New))
	fmt.Fprintf(tw, "  to fetch (stored)\t%s\t\n", human(st.NewStored))
	fmt.Fprintf(tw, "changed runs\t%d\tlargest %s\n", st.ChangedRuns, human(st.LargestRun))
	tw.Flush()
	fmt.Fprintln(stdout)
	tw = tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PATH\tBASE\tSIZE\tSAME\tMOVED\tNEW\tRUNS")
	for _, f := range st.Files {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\n", f.Path, f.Base, human(f.Size), human(f.Same), human(f.Moved), human(f.New), f.ChangedRuns)
	}
	tw.Flush()
	for _, f := range st.Files {
		for _, r := range f.Changed {
			fmt.Fprintf(stdout, "changed %s offset %d length %d (%s)\n", f.Path, r[0], r[1], human(r[1]))
		}
	}
	return 0
}
