package peer

import (
	"bytes"
	"context"
	"math/rand"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ai-computing/onggi/pkg/backend"
	bfs "github.com/ai-computing/onggi/pkg/backend/fs"
	"github.com/ai-computing/onggi/pkg/chunker"
	"github.com/ai-computing/onggi/pkg/codec"
	"github.com/ai-computing/onggi/pkg/digest"
	"github.com/ai-computing/onggi/pkg/store"
)

func open(t *testing.T, dir string) *store.Store {
	t.Helper()
	b, err := bfs.Open(dir, backend.DurabilityNone)
	if err != nil {
		t.Fatal(err)
	}
	sub := store.DefaultSub
	s, err := store.OpenWith(context.Background(), b, store.Create{Chunker: chunker.Default, Sub: &sub, PackSize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func random(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

type fixture struct {
	storeDir string
	src      string
	m        digest.Digest
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	f := fixture{storeDir: t.TempDir(), src: t.TempDir()}
	os.WriteFile(filepath.Join(f.src, "model.rbln"), random(1, 3<<20), 0o644)
	os.WriteFile(filepath.Join(f.src, "config.json"), []byte(`{"layers": 2}`), 0o644)
	var err error
	f.m, _, err = open(t, f.storeDir).Push(context.Background(), f.src, store.PushOptions{Compress: codec.ModeAuto})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func same(t *testing.T, a, b string) {
	t.Helper()
	for _, n := range []string{"model.rbln", "config.json"} {
		x, _ := os.ReadFile(filepath.Join(a, n))
		y, err := os.ReadFile(filepath.Join(b, n))
		if err != nil || !bytes.Equal(x, y) {
			t.Fatalf("%s differs: %v", n, err)
		}
	}
}

// node A pulls from the store into its cache and serves it; node B pulls with A as its peer
func warmPeer(t *testing.T, f fixture) (*httptest.Server, string) {
	t.Helper()
	cacheA := t.TempDir()
	if _, err := open(t, f.storeDir).Pull(context.Background(), f.m, t.TempDir(), store.PullOptions{Cache: open(t, cacheA)}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(&Server{Open: func(context.Context) (*store.Store, error) { return open(t, cacheA), nil }})
	t.Cleanup(srv.Close)
	return srv, cacheA
}

func TestPullFromPeer(t *testing.T) {
	f := newFixture(t)
	srv, _ := warmPeer(t, f)
	out := t.TempDir()
	st, err := open(t, f.storeDir).Pull(context.Background(), f.m, out, store.PullOptions{
		Cache: open(t, t.TempDir()), Peers: []store.ChunkSource{&Client{URL: srv.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	same(t, f.src, out)
	t.Logf("store %d B, peer %d B in %d chunks, cached %d B", st.ReadBytes, st.PeerBytes, st.PeerChunks, st.Cached)
	if st.ReadBytes != 0 || st.PeerBytes == 0 || st.Cached != st.PeerBytes {
		t.Fatalf("pull with a warm peer: %+v", st)
	}
}

// corruptSource flips a byte in every chunk it delivers.
type corruptSource struct{ store.ChunkSource }

func (c corruptSource) Fetch(ctx context.Context, ds []digest.Digest, fn func(digest.Digest, []byte, codec.ID, uint32) error) error {
	return c.ChunkSource.Fetch(ctx, ds, func(d digest.Digest, b []byte, id codec.ID, n uint32) error {
		bad := append([]byte(nil), b...)
		bad[len(bad)/2] ^= 0xff
		return fn(d, bad, id, n)
	})
}

func TestCorruptPeerFallsBackToTheStore(t *testing.T) {
	f := newFixture(t)
	srv, _ := warmPeer(t, f)
	out := t.TempDir()
	st, err := open(t, f.storeDir).Pull(context.Background(), f.m, out, store.PullOptions{
		Peers: []store.ChunkSource{corruptSource{&Client{URL: srv.URL}}}})
	if err != nil {
		t.Fatal(err)
	}
	same(t, f.src, out)
	if st.PeerChunks != 0 || st.ReadBytes == 0 {
		t.Fatalf("corrupt peer: %+v", st)
	}
}

func TestDeadPeerFallsBackToTheStore(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(&Server{Open: func(context.Context) (*store.Store, error) { return open(t, t.TempDir()), nil }})
	url := srv.URL
	srv.Close()
	out := t.TempDir()
	st, err := open(t, f.storeDir).Pull(context.Background(), f.m, out, store.PullOptions{Peers: []store.ChunkSource{&Client{URL: url}}})
	if err != nil {
		t.Fatal(err)
	}
	same(t, f.src, out)
	if st.PeerBytes != 0 || st.ReadBytes == 0 {
		t.Fatalf("dead peer: %+v", st)
	}
}

// Two warm peers: every chunk is read once, and the rendezvous choice spreads them over both.
func TestTwoPeersShareTheLoad(t *testing.T) {
	f := newFixture(t)
	a, _ := warmPeer(t, f)
	b, _ := warmPeer(t, f)
	out := t.TempDir()
	ca, cb := &countingSource{ChunkSource: &Client{URL: a.URL}}, &countingSource{ChunkSource: &Client{URL: b.URL}}
	st, err := open(t, f.storeDir).Pull(context.Background(), f.m, out, store.PullOptions{Peers: []store.ChunkSource{ca, cb}})
	if err != nil {
		t.Fatal(err)
	}
	same(t, f.src, out)
	t.Logf("peer a %d chunks, peer b %d chunks, store %d B", ca.n, cb.n, st.ReadBytes)
	if st.ReadBytes != 0 || ca.n == 0 || cb.n == 0 || ca.n+cb.n != int(st.PeerChunks) {
		t.Fatalf("load not shared: a %d, b %d, %+v", ca.n, cb.n, st)
	}
}

type countingSource struct {
	store.ChunkSource
	n int
}

func (c *countingSource) Fetch(ctx context.Context, ds []digest.Digest, fn func(digest.Digest, []byte, codec.ID, uint32) error) error {
	return c.ChunkSource.Fetch(ctx, ds, func(d digest.Digest, b []byte, id codec.ID, n uint32) error {
		c.n++
		return fn(d, b, id, n)
	})
}

func TestServerRejectsBadRequests(t *testing.T) {
	srv := httptest.NewServer(&Server{Open: func(context.Context) (*store.Store, error) { return open(t, t.TempDir()), nil }})
	defer srv.Close()
	for _, c := range []struct {
		method, path string
		body         []byte
		want         int
	}{
		{"GET", "/v1/has", nil, 405},
		{"POST", "/v1/nope", nil, 404},
		{"POST", "/v1/has", []byte("short"), 400},
	} {
		req := httptest.NewRequest(c.method, c.path, bytes.NewReader(c.body))
		rec := httptest.NewRecorder()
		(&Server{Open: func(context.Context) (*store.Store, error) { return open(t, t.TempDir()), nil }}).ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, rec.Code, c.want)
		}
	}
}
