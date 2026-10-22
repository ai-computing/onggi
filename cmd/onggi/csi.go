package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/ai-computing/onggi/pkg/backend"
	bfs "github.com/ai-computing/onggi/pkg/backend/fs"
	"github.com/ai-computing/onggi/pkg/csi"
	"github.com/ai-computing/onggi/pkg/digest"
	"github.com/ai-computing/onggi/pkg/store"
)

// csiNode runs the CSI node plugin (ADR-0004, #27). The store and its credentials come from the
// plugin's own flags and environment (ONGGI_STORE, AWS_*), never from pods.
func csiNode(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("csi", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sf := addStoreFlags(fs)
	endpoint := fs.String("endpoint", envOr("CSI_ENDPOINT", "unix:///csi/csi.sock"), "CSI endpoint (env CSI_ENDPOINT)")
	nodeID := fs.String("node-id", os.Getenv("NODE_NAME"), "node name (env NODE_NAME)")
	dataDir := fs.String("data-dir", "/var/lib/onggi", "node directory for materialized artifacts and leases")
	cacheDir := fs.String("cache", "", "node chunk cache (default <data-dir>/cache; none: no cache)")
	workers := fs.Int("workers", 16, "ranges read at once per pull")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: onggi csi --store S [--endpoint unix:///csi/csi.sock] [--node-id N] [--data-dir DIR] [--cache DIR]")
		return 2
	}
	logger := log.New(stderr, "", log.LstdFlags|log.Lmicroseconds)
	if *nodeID == "" {
		logger.Print("no node id: pass --node-id or set NODE_NAME")
		return 2
	}
	ctx := context.Background()
	s, refs, err := sf.open(ctx)
	if err != nil {
		logger.Print("store: ", err)
		return 1
	}
	tmp := filepath.Join(*dataDir, "tmp")
	var cache *store.Store
	if *cacheDir != "none" {
		if *cacheDir == "" {
			*cacheDir = filepath.Join(*dataDir, "cache")
		}
		cb, err := bfs.Open(*cacheDir, backend.DurabilityNone)
		if err == nil {
			cache, err = store.OpenWith(ctx, cb, store.Create{Chunker: s.Config.Chunker, Sub: s.Config.Sub,
				SubKinds: s.Config.SubKinds, PackSize: s.Config.PackSize})
		}
		if err == nil {
			err = cache.LoadIndex(ctx)
		}
		if err != nil {
			logger.Print("node cache: ", err)
			return 1
		}
		chunks, packs := cache.Stats()
		logger.Printf("node cache %s: %d chunks in %d packs", *cacheDir, chunks, packs)
	}
	m := &csi.Materializer{Root: *dataDir, Pull: csi.StorePuller(s, cache, tmp, *workers), Logf: logger.Printf}
	if err := m.Init(); err != nil {
		logger.Print("data dir: ", err)
		return 1
	}
	n := &csi.Node{
		Version: version,
		NodeID:  *nodeID,
		M:       m,
		Resolve: func(ctx context.Context, ref string) (digest.Digest, error) { return store.Resolve(ctx, refs, ref) },
		Mounter: csi.NewMounter(),
		Logf:    logger.Printf,
	}
	l, err := csi.Listen(*endpoint)
	if err != nil {
		logger.Print("endpoint: ", err)
		return 1
	}
	srv := csi.NewServer(n)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, os.Interrupt)
	go func() {
		<-sig
		logger.Print("stopping")
		srv.GracefulStop()
	}()
	logger.Printf("onggi %s CSI node plugin %s on %s, node %s, data %s", version, csi.DriverName, *endpoint, *nodeID, *dataDir)
	if err := srv.Serve(l); err != nil {
		logger.Print(err)
		return 1
	}
	os.Remove(l.Addr().String())
	return 0
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
