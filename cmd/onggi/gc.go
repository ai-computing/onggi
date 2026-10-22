package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ai-computing/onggi/pkg/backend"
	"github.com/ai-computing/onggi/pkg/digest"
	"github.com/ai-computing/onggi/pkg/store"
)

func rmRef(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("rm-ref", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sf := addStoreFlags(fs)
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: onggi rm-ref --store S NAME")
		return 2
	}
	ctx := context.Background()
	_, refs, err := sf.open(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "onggi:", err)
		return 1
	}
	name := fs.Arg(0)
	old, err := refs.GetRef(ctx, name)
	if err == nil {
		err = refs.SetRef(ctx, name, old, "")
	}
	if err != nil {
		fmt.Fprintln(stderr, "onggi:", err)
		if errors.Is(err, backend.ErrNotFound) {
			return 1
		}
		return 1
	}
	fmt.Fprintf(stdout, "deleted ref %s (was %s)\n", name, old)
	return 0
}

func gc(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sf := addStoreFlags(fs)
	grace := fs.Duration("grace", 24*time.Hour, "time between the run that records a candidate and the run that deletes it")
	leaseTTL := fs.Duration("lease-ttl", store.DefaultLeaseTTL, "leases older than this are treated as left by crashed pushes")
	keep := fs.String("keep", "", "comma-separated manifest digests kept in addition to the refs")
	dry := fs.Bool("dry-run", false, "report only")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: onggi gc --store S [--grace 24h] [--keep sha256:...,...] [--dry-run]")
		return 2
	}
	ctx := context.Background()
	s, refs, err := sf.open(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "onggi:", err)
		return 1
	}
	o := store.GCOptions{Grace: *grace, LeaseTTL: *leaseTTL, DryRun: *dry}
	for _, k := range strings.Split(*keep, ",") {
		if k = strings.TrimSpace(k); k == "" {
			continue
		}
		d, err := digest.Parse(k)
		if err != nil {
			fmt.Fprintln(stderr, "onggi: --keep:", err)
			return 2
		}
		o.Keep = append(o.Keep, d)
	}
	st, err := s.GC(ctx, refs, o)
	if err != nil {
		fmt.Fprintln(stderr, "onggi:", err)
		return 1
	}
	if *asJSON {
		return writeJSON(stdout, stderr, st)
	}
	fmt.Fprintf(stdout, "phase      %s (candidates recorded %s)\nroots      %d refs and kept manifests, %d live manifests\npacks      %d, %d live; dead bytes inside live packs %s\ncandidates %d objects, %s\ndeleted    %d objects, %s\nrewritten  %d composite tables\n",
		st.Phase, st.MarkedAt, st.Roots, st.LiveManifests, st.Packs, st.LivePacks, human(st.DeadInLive),
		st.Candidates, human(st.CandidateBytes), st.Deleted, human(st.DeletedBytes), st.Rewritten)
	switch st.Phase {
	case "marked":
		fmt.Fprintf(stdout, "run again after %s to delete the candidates that are still unreferenced\n", *grace)
	case "waiting":
		fmt.Fprintln(stdout, "the grace period has not passed since the candidates were recorded")
	case "blocked":
		fmt.Fprintf(stdout, "%d live lease(s): a push is running; nothing deleted\n", st.Leases)
	}
	return 0
}
