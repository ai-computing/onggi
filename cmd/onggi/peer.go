package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/ai-computing/onggi/pkg/backend"
	bfs "github.com/ai-computing/onggi/pkg/backend/fs"
	"github.com/ai-computing/onggi/pkg/chunker"
	"github.com/ai-computing/onggi/pkg/peer"
	"github.com/ai-computing/onggi/pkg/store"
)

// peerServe serves a node cache to other nodes (#34).
func peerServe(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("peer serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cacheDir := fs.String("cache", "", "the node cache directory to serve")
	listen := fs.String("listen", ":7070", "address to listen on")
	refresh := fs.Duration("refresh", 30*time.Second, "how often to reload the cache's index")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *cacheDir == "" {
		fmt.Fprintln(stderr, "usage: onggi peer serve --cache DIR [--listen :7070]")
		return 2
	}
	logger := log.New(stderr, "", log.LstdFlags)
	dir := *cacheDir
	srv := &peer.Server{Refresh: *refresh, Open: func(ctx context.Context) (*store.Store, error) {
		b, err := bfs.Open(dir, backend.DurabilityNone)
		if err != nil {
			return nil, err
		}
		return store.Open(ctx, b, chunker.Default, 0)
	}}
	logger.Printf("serving node cache %s on %s", dir, *listen)
	if err := http.ListenAndServe(*listen, srv); err != nil {
		logger.Print(err)
		return 1
	}
	return 0
}

// peerClients parses --peers: comma-separated base URLs.
func peerClients(list string) []store.ChunkSource {
	var out []store.ChunkSource
	for _, u := range strings.Split(list, ",") {
		if u = strings.TrimRight(strings.TrimSpace(u), "/"); u != "" {
			out = append(out, &peer.Client{URL: u, HTTP: &http.Client{Timeout: 10 * time.Minute}})
		}
	}
	return out
}
