// Benchmark-only caller; production Store.Pull is unchanged.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ai-computing/onggi/pkg/backend"
	bfs "github.com/ai-computing/onggi/pkg/backend/fs"
	"github.com/ai-computing/onggi/pkg/digest"
	"github.com/ai-computing/onggi/pkg/store"
)

func disjointPaths(storeDir, cacheDir, baseDir, dest string) error {
	var paths []string
	for _, dir := range []string{storeDir, cacheDir, baseDir} {
		path, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return err
		}
		path, err = filepath.Abs(path)
		if err != nil {
			return err
		}
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			return fmt.Errorf("expected existing source directory: %s", dir)
		}
		paths = append(paths, path)
	}
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		return fmt.Errorf("output must be new: %s", dest)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(dest))
	if err != nil {
		return err
	}
	output, err := filepath.Abs(filepath.Join(parent, filepath.Base(dest)))
	if err != nil {
		return err
	}
	paths = append(paths, output)
	for i, a := range paths {
		for j, b := range paths {
			if i == j {
				continue
			}
			rel, err := filepath.Rel(a, b)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return fmt.Errorf("paths overlap: %s and %s", a, b)
			}
		}
	}
	return nil
}

func runThree(args []string, out, stderr io.Writer) int {
	f := flag.NewFlagSet("three-arm-call", flag.ContinueOnError)
	f.SetOutput(stderr)
	storeDir := f.String("store", "", "existing immutable artifact store")
	cacheDir := f.String("cache", "", "independent complete target cache")
	baseRef := f.String("base", "", "truthful base manifest digest")
	baseDir := f.String("base-dir", "", "independent verified base")
	policy := f.String("policy", "", "P reconstruction, C intact copy, R consume")
	workers := f.Int("workers", 8, "concurrent readers")
	if err := f.Parse(args); err != nil {
		return 2
	}
	if f.NArg() != 2 || *storeDir == "" || *cacheDir == "" || *baseDir == "" || *baseRef == "" || *workers < 1 || (*policy != "P" && *policy != "C" && *policy != "R") {
		fmt.Fprintln(stderr, "all source paths, base digest, P/C/R policy and target/output are required")
		return 2
	}
	ctx := context.Background()
	err := func() error {
		md, err := digest.Parse(f.Arg(0))
		if err != nil {
			return err
		}
		bd, err := digest.Parse(*baseRef)
		if err != nil {
			return err
		}
		if err := disjointPaths(*storeDir, *cacheDir, *baseDir, f.Arg(1)); err != nil {
			return err
		}
		b, err := bfs.Open(*storeDir, backend.DurabilityNone)
		if err != nil {
			return err
		}
		s, err := store.OpenWith(ctx, b, store.Create{})
		if err != nil {
			return err
		}
		cb, err := bfs.Open(*cacheDir, backend.DurabilityNone)
		if err != nil {
			return err
		}
		cache, err := store.OpenWith(ctx, cb, store.Create{Chunker: s.Config.Chunker, Sub: s.Config.Sub,
			SubKinds: s.Config.SubKinds, PackSize: s.Config.PackSize})
		if err != nil {
			return err
		}
		calls, admitted := 0, false
		o := store.PullOptions{Workers: *workers, Cache: cache}
		if *policy != "P" {
			o.Bases = []store.Base{{Manifest: bd, Dir: *baseDir}}
			o.Claim = func(b store.Base) (string, bool) {
				calls++
				if *policy == "C" {
					return "", false
				}
				admitted = true
				return b.Dir, true
			}
		}
		st, err := s.Pull(ctx, md, f.Arg(1), o)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(map[string]any{
			"policy": *policy, "manifest": md.String(), "claim_calls": calls, "admitted": admitted, "stats": st,
		})
	}()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func main() { os.Exit(runThree(os.Args[1:], os.Stdout, os.Stderr)) }
