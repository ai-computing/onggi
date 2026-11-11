package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/ai-computing/onggi/pkg/casync"
	"github.com/ai-computing/onggi/pkg/csi"
	"github.com/ai-computing/onggi/pkg/digest"
	"github.com/ai-computing/onggi/pkg/store"
)

const casyncUsage = `onggi casync — artifacts in casync format (desync chunk store), with state-aware in-place reuse

  onggi casync publish --store S --index I <dir>        chunk dir into S; print the manifest digest
  onggi casync pull    --store S --index I [--attest A] <manifest> <dir>
        [--base M --base-dir D]     take base M over (Keep): write only differing chunks; kept chunks are
                                    read back unless A attests the base unchanged since it was written
        [--copy M --copy-dir D]     read base M where it is (Copy); it stays intact
        [--verify-kept] [--no-sync] [--workers N] [--log]
  onggi casync verify  --store S --index I <manifest> <dir> hash-check every chunk of dir
  onggi casync attest  --store S --index I --attest A <manifest> <dir>   verify dir, then record it as unchanged
`

func casyncCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprint(stderr, casyncUsage)
		return 2
	}
	fs := flag.NewFlagSet("casync "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	storeDir := fs.String("store", "", "desync local chunk store")
	indexDir := fs.String("index", "", "directory of artifact indexes")
	attestDir := fs.String("attest", "", "directory of attestations (none: kept chunks are always read back)")
	workers := fs.Int("workers", 12, "parallel chunk reads and writes")
	base := fs.String("base", "", "Keep: manifest of the artifact to take over")
	baseDir := fs.String("base-dir", "", "Keep: directory holding the base's files")
	cp := fs.String("copy", "", "Copy: manifest of an artifact to read where it is")
	copyDir := fs.String("copy-dir", "", "Copy: directory holding that artifact's files")
	verifyKept := fs.Bool("verify-kept", false, "read back kept chunks even if the base is attested unchanged")
	noSync := fs.Bool("no-sync", false, "do not sync written files (desync's default behaviour)")
	logPhases := fs.Bool("log", false, "log phase durations to stderr")
	if err := fs.Parse(args[1:]); err != nil || *storeDir == "" || *indexDir == "" {
		fmt.Fprint(stderr, casyncUsage)
		return 2
	}
	e, err := casync.New(*storeDir, *indexDir, *attestDir, *workers)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	e.Durable = !*noSync
	e.VerifyKept = *verifyKept
	if *logPhases {
		l := log.New(stderr, "", log.Lmicroseconds)
		e.Logf = l.Printf
	}
	ctx := context.Background()
	switch args[0] {
	case "publish":
		if fs.NArg() != 1 {
			fmt.Fprint(stderr, casyncUsage)
			return 2
		}
		start := time.Now()
		d, err := e.Publish(ctx, fs.Arg(0))
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "%s\t%.2fs\n", d, time.Since(start).Seconds())
		return 0
	case "pull", "verify", "attest":
		if fs.NArg() != 2 {
			fmt.Fprint(stderr, casyncUsage)
			return 2
		}
		m, err := digest.Parse(fs.Arg(0))
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		dest := fs.Arg(1)
		if args[0] != "pull" {
			if err := e.Verify(ctx, m, dest); err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			if args[0] == "attest" {
				if err := e.Attest(m, dest); err != nil {
					fmt.Fprintln(stderr, err)
					return 1
				}
			}
			return 0
		}
		if err := os.MkdirAll(dest, 0o755); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		var offer *csi.Offer
		if *base != "" {
			b, err := digest.Parse(*base)
			if err != nil {
				fmt.Fprintln(stderr, err)
				return 2
			}
			offer = &csi.Offer{Bases: []store.Base{{Manifest: b, Dir: *baseDir}},
				Claim: func(sb store.Base) (string, bool) { return sb.Dir, true }}
		}
		if *cp != "" {
			c, err := digest.Parse(*cp)
			if err != nil {
				fmt.Fprintln(stderr, err)
				return 2
			}
			if offer == nil {
				offer = &csi.Offer{}
			}
			offer.Readable, offer.BaseCopy = []store.Base{{Manifest: c, Dir: *copyDir}}, store.BaseCopyAlways
		}
		st, err := e.Pull(ctx, m, dest, offer)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		js, _ := json.Marshal(st)
		fmt.Fprintln(stdout, string(js))
		return 0
	}
	fmt.Fprint(stderr, casyncUsage)
	return 2
}
