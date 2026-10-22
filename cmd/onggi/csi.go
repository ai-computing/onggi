package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ai-computing/onggi/pkg/backend"
	bfs "github.com/ai-computing/onggi/pkg/backend/fs"
	"github.com/ai-computing/onggi/pkg/csi"
	"github.com/ai-computing/onggi/pkg/digest"
	"github.com/ai-computing/onggi/pkg/peer"
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
	evictAfter := fs.Duration("evict-after", 24*time.Hour, "evict materialized artifacts no volume used for this long (0: never)")
	gcEvery := fs.Duration("gc-every", time.Hour, "how often to evict and collect the node cache (0: never)")
	keepUsed := fs.Duration("keep-used", 7*24*time.Hour, "the node cache keeps what artifacts used within this period need")
	gcGrace := fs.Duration("gc-grace", time.Hour, "grace period of the node cache's GC")
	peerListen := fs.String("peer-listen", "", "serve the node cache to other nodes' pulls on this address, e.g. :7070 (#37)")
	peers := fs.String("peers", "", "comma-separated peer URLs asked for chunks before the store")
	peersDNS := fs.String("peers-dns", "", "host:port whose name resolves to the peers, e.g. a headless Service; re-resolved every 30 s")
	selfIP := fs.String("pod-ip", os.Getenv("POD_IP"), "this plugin's address, left out of --peers-dns (env POD_IP)")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: onggi csi --store S [--endpoint unix:///csi/csi.sock] [--node-id N] [--data-dir DIR] [--cache DIR] [--peer-listen :7070] [--peers-dns H:P]")
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
	// the cache is opened afresh for each pull and each GC round: GC deletes packs, and a store
	// opened before would still list them in its index
	openCache := func(context.Context) (*store.Store, error) { return nil, nil }
	if *cacheDir != "none" {
		if *cacheDir == "" {
			*cacheDir = filepath.Join(*dataDir, "cache")
		}
		dir := *cacheDir
		openCache = func(ctx context.Context) (*store.Store, error) {
			cb, err := bfs.Open(dir, backend.DurabilityNone)
			if err != nil {
				return nil, err
			}
			return store.OpenWith(ctx, cb, store.Create{Chunker: s.Config.Chunker, Sub: s.Config.Sub,
				SubKinds: s.Config.SubKinds, PackSize: s.Config.PackSize})
		}
		cache, err := openCache(ctx)
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
	var peerList func(context.Context) []store.ChunkSource
	if static := peerClients(*peers); len(static) > 0 || *peersDNS != "" {
		d := &peer.Discovery{Name: *peersDNS, Self: []string{*selfIP}}
		peerList = func(ctx context.Context) []store.ChunkSource {
			if *peersDNS == "" {
				return static
			}
			return append(append([]store.ChunkSource(nil), static...), d.Peers(ctx)...)
		}
	}
	m := &csi.Materializer{Root: *dataDir, Pull: csi.StorePullerPeers(s, openCache, peerList, tmp, *workers), Logf: logger.Printf}
	if err := m.Init(); err != nil {
		logger.Print("data dir: ", err)
		return 1
	}
	mounter := csi.NewMounter()
	n := &csi.Node{
		Version: version,
		NodeID:  *nodeID,
		M:       m,
		Resolve: func(ctx context.Context, ref string) (digest.Digest, error) { return store.Resolve(ctx, refs, ref) },
		Mounter: mounter,
		Logf:    logger.Printf,
	}
	if *gcEvery > 0 {
		go func() {
			for range time.Tick(*gcEvery) {
				nodeGC(ctx, logger, m, mounter, s, openCache, *cacheDir != "none", *evictAfter, *keepUsed, *gcGrace, tmp)
			}
		}()
	}
	if *peerListen != "" && *cacheDir != "none" {
		ps := &peer.Server{Open: openCache}
		go func() {
			logger.Printf("serving the node cache to peers on %s", *peerListen)
			if err := http.ListenAndServe(*peerListen, ps); err != nil {
				logger.Print("peer server: ", err)
			}
		}()
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

// nodeGC is one round of node-side cleanup (#33): evict idle artifacts, then collect the node
// cache with the artifacts still materialized and the recently used ones as roots.
func nodeGC(ctx context.Context, logger *log.Logger, m *csi.Materializer, mounter csi.Mounter, s *store.Store,
	openCache func(context.Context) (*store.Store, error), withCache bool, evictAfter, keepUsed, grace time.Duration, tmp string) {
	if evictAfter > 0 {
		if _, err := m.Evict(time.Now(), evictAfter, mounter.Unmount); err != nil {
			logger.Print("evict: ", err)
		}
	}
	if !withCache {
		return
	}
	keep, err := m.Materialized()
	if err != nil {
		logger.Print("node GC: ", err)
		return
	}
	cache, err := openCache(ctx)
	if err != nil {
		logger.Print("node GC: ", err)
		return
	}
	st, err := cache.GC(ctx, nil, store.GCOptions{RootStore: s, Keep: keep, KeepUsed: keepUsed, Grace: grace,
		RepackDead: 0.25, TempDir: tmp})
	if err != nil {
		logger.Print("node GC: ", err)
		return
	}
	logger.Printf("node GC: %s; %d roots; %d candidates; deleted %d objects (%d B); repacked %d packs",
		st.Phase, st.Roots, st.Candidates, st.Deleted, st.DeletedBytes, st.Repacked)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
