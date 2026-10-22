package peer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ai-computing/onggi/pkg/store"
)

func names(ps []store.ChunkSource) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Name())
	}
	return out
}

func TestDiscoveryLeavesOutSelfAndRefreshes(t *testing.T) {
	answer := []string{"10.0.0.3", "10.0.0.1", "10.0.0.2"}
	var fail bool
	lookups := 0
	d := &Discovery{Name: "onggi-peers.ns.svc:7070", Self: []string{"10.0.0.2"}, TTL: time.Hour,
		Lookup: func(_ context.Context, host string) ([]string, error) {
			lookups++
			if host != "onggi-peers.ns.svc" {
				t.Fatalf("looked up %q", host)
			}
			if fail {
				return nil, errors.New("no such host")
			}
			return answer, nil
		}}
	ctx := context.Background()
	got := names(d.Peers(ctx))
	want := []string{"http://10.0.0.1:7070", "http://10.0.0.3:7070"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("peers %v, want %v", got, want)
	}
	d.Peers(ctx)
	if lookups != 1 {
		t.Fatalf("%d lookups within the TTL", lookups)
	}
	// after the TTL: a failed lookup keeps the last answer, a new one replaces it
	d.at = time.Time{}
	fail = true
	if got := names(d.Peers(ctx)); len(got) != 2 {
		t.Fatalf("failed lookup dropped the peers: %v", got)
	}
	d.at, fail, answer = time.Time{}, false, []string{"10.0.0.2", "10.0.0.4"}
	if got := names(d.Peers(ctx)); len(got) != 1 || got[0] != "http://10.0.0.4:7070" {
		t.Fatalf("after a change: %v", got)
	}
}

func TestDiscoveryIPv6(t *testing.T) {
	d := &Discovery{Name: "peers:7070", Lookup: func(context.Context, string) ([]string, error) {
		return []string{"fd00::5"}, nil
	}}
	if got := names(d.Peers(context.Background())); len(got) != 1 || got[0] != "http://[fd00::5]:7070" {
		t.Fatalf("%v", got)
	}
}
