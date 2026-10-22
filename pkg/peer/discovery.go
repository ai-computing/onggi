package peer

import (
	"context"
	"net"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/ai-computing/onggi/pkg/store"
)

// Discovery finds peers through DNS (#37): Name is host:port, typically a headless Service whose
// name resolves to the addresses of every node plugin. It resolves again every TTL (default
// 30 s) and leaves out the node's own addresses, so a plugin never asks itself.
type Discovery struct {
	Name   string
	Self   []string // own addresses, e.g. the pod IP
	TTL    time.Duration
	HTTP   *http.Client
	Lookup func(ctx context.Context, host string) ([]string, error) // nil: net.DefaultResolver.LookupHost
	Logf   func(format string, args ...any)                            // nil: no log; called when the peers change

	mu    sync.Mutex
	cur   []store.ChunkSource
	addrs []string
	at    time.Time
}

// Peers returns the current peers. A failed lookup keeps the last answer: peers are an
// optimization, and the store serves what they do not.
func (d *Discovery) Peers(ctx context.Context) []store.ChunkSource {
	d.mu.Lock()
	defer d.mu.Unlock()
	ttl := d.TTL
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	if !d.at.IsZero() && time.Since(d.at) < ttl {
		return d.cur
	}
	d.at = time.Now()
	host, port, err := net.SplitHostPort(d.Name)
	if err != nil {
		return d.cur
	}
	lookup := d.Lookup
	if lookup == nil {
		lookup = net.DefaultResolver.LookupHost
	}
	ips, err := lookup(ctx, host)
	if err != nil {
		return d.cur
	}
	var addrs []string
	for _, ip := range ips {
		if !slices.Contains(d.Self, ip) {
			addrs = append(addrs, ip)
		}
	}
	slices.Sort(addrs)
	if slices.Equal(addrs, d.addrs) {
		return d.cur
	}
	hc := d.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Minute}
	}
	cur := make([]store.ChunkSource, 0, len(addrs))
	for _, a := range addrs {
		cur = append(cur, &Client{URL: "http://" + net.JoinHostPort(a, port), HTTP: hc})
	}
	d.cur, d.addrs = cur, addrs
	if d.Logf != nil {
		d.Logf("peers from %s: %d %v", d.Name, len(addrs), addrs)
	}
	return cur
}
