package csi

import (
	"bytes"
	"context"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ai-computing/onggi/pkg/backend"
	bfs "github.com/ai-computing/onggi/pkg/backend/fs"
	"github.com/ai-computing/onggi/pkg/chunker"
	"github.com/ai-computing/onggi/pkg/codec"
	"github.com/ai-computing/onggi/pkg/digest"
	"github.com/ai-computing/onggi/pkg/store"
)

func random(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func openStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	b, err := bfs.Open(dir, backend.DurabilityNone)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.OpenWith(context.Background(), b, store.Create{
		Chunker: chunker.Params{Min: 4 << 10, Avg: 16 << 10, Max: 64 << 10}, PackSize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// fixture pushes two artifacts that share most bytes into an fs store.
type fixture struct {
	s          *store.Store
	cache      *store.Store
	a, b       digest.Digest
	srcA, srcB string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	f := &fixture{s: openStore(t, t.TempDir()), cache: openStore(t, t.TempDir())}
	shared := random(1, 3<<20)
	f.srcA, f.srcB = t.TempDir(), t.TempDir()
	write := func(p string, data []byte) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(f.srcA, "model.bin"), shared)
	write(filepath.Join(f.srcA, "sub", "config.json"), []byte(`{"a":1}`))
	write(filepath.Join(f.srcB, "model.bin"), append(append([]byte(nil), shared...), random(2, 100_000)...))
	var err error
	if f.a, _, err = f.s.Push(ctx, f.srcA, store.PushOptions{Compress: codec.ModeAuto}); err != nil {
		t.Fatal(err)
	}
	if f.b, _, err = f.s.Push(ctx, f.srcB, store.PushOptions{Compress: codec.ModeAuto}); err != nil {
		t.Fatal(err)
	}
	return f
}

// counting wraps a puller and records each pull's stats.
type counting struct {
	Puller
	mu    sync.Mutex
	pulls []store.PullStats
	gate  chan struct{} // when set, pulls wait for it to close
}

func (c *counting) pull(ctx context.Context, m digest.Digest, dest string) (store.PullStats, error) {
	if c.gate != nil {
		<-c.gate
	}
	st, err := c.Puller(ctx, m, dest)
	c.mu.Lock()
	c.pulls = append(c.pulls, st)
	c.mu.Unlock()
	return st, err
}

func sameTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		want, _ := os.ReadFile(p)
		got, err := os.ReadFile(filepath.Join(dst, rel))
		if err != nil {
			return err
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func newMaterializer(t *testing.T, f *fixture) (*Materializer, *counting) {
	t.Helper()
	root := t.TempDir()
	c := &counting{Puller: StorePuller(f.s, f.cache, filepath.Join(root, "tmp"), 4)}
	m := &Materializer{Root: root, Pull: c.pull, Logf: t.Logf}
	if err := m.Init(); err != nil {
		t.Fatal(err)
	}
	return m, c
}

// Many concurrent requests for one artifact run one pull and all get the same directory.
func TestEnsureSingleFlight(t *testing.T) {
	f := newFixture(t)
	m, c := newMaterializer(t, f)
	c.gate = make(chan struct{})
	const n = 8
	dirs := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			dirs[i], errs[i] = m.Ensure(context.Background(), f.a)
		}(i)
	}
	time.Sleep(50 * time.Millisecond) // let every caller join the flight
	close(c.gate)
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil || dirs[i] != m.Dir(f.a) {
			t.Fatalf("call %d: %q %v", i, dirs[i], errs[i])
		}
	}
	if len(c.pulls) != 1 {
		t.Fatalf("%d pulls, want 1", len(c.pulls))
	}
	sameTree(t, f.srcA, m.Dir(f.a))
	// a later request finds the directory and pulls nothing
	if _, err := m.Ensure(context.Background(), f.a); err != nil || len(c.pulls) != 1 {
		t.Fatalf("repeat: %v, %d pulls", err, len(c.pulls))
	}
}

// The second artifact reads only its new bytes from the store, the rest from the node cache.
func TestEnsureUsesNodeCache(t *testing.T) {
	f := newFixture(t)
	m, c := newMaterializer(t, f)
	if _, err := m.Ensure(context.Background(), f.a); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Ensure(context.Background(), f.b); err != nil {
		t.Fatal(err)
	}
	sameTree(t, f.srcB, m.Dir(f.b))
	st := c.pulls[1]
	if st.CacheBytes == 0 || st.ReadBytes*10 > st.Bytes {
		t.Fatalf("second artifact: %d B from the store, %d B from the cache, %d B written", st.ReadBytes, st.CacheBytes, st.Bytes)
	}
}

// A failed pull publishes nothing and leaves no temp directory; the next request pulls again.
func TestEnsureFailedPullPublishesNothing(t *testing.T) {
	f := newFixture(t)
	m, c := newMaterializer(t, f)
	good := c.Puller
	c.Puller = func(ctx context.Context, d digest.Digest, dest string) (store.PullStats, error) {
		os.WriteFile(filepath.Join(dest, "half"), []byte("x"), 0o644)
		return store.PullStats{}, errors.New("store went away")
	}
	if _, err := m.Ensure(context.Background(), f.a); err == nil {
		t.Fatal("want an error")
	}
	if isDir(m.Dir(f.a)) {
		t.Fatal("a failed pull published a directory")
	}
	if ents, _ := os.ReadDir(filepath.Join(m.Root, "tmp")); len(ents) != 0 {
		t.Fatalf("tmp holds %d entries", len(ents))
	}
	c.Puller = good
	if _, err := m.Ensure(context.Background(), f.a); err != nil {
		t.Fatal(err)
	}
	sameTree(t, f.srcA, m.Dir(f.a))
}

// A caller whose context ends gets its error; the pull goes on and a retry finds the result.
func TestEnsureOutlivesCaller(t *testing.T) {
	f := newFixture(t)
	m, c := newMaterializer(t, f)
	c.gate = make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := m.Ensure(ctx, f.a); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want deadline exceeded", err)
	}
	close(c.gate)
	if _, err := m.Ensure(context.Background(), f.a); err != nil {
		t.Fatal(err)
	}
	if len(c.pulls) != 1 {
		t.Fatalf("%d pulls, want 1", len(c.pulls))
	}
}

// A directory published by another process meanwhile wins; ours is discarded.
func TestEnsureRenameRace(t *testing.T) {
	f := newFixture(t)
	m, c := newMaterializer(t, f)
	good := c.Puller
	c.Puller = func(ctx context.Context, d digest.Digest, dest string) (store.PullStats, error) {
		other := filepath.Join(m.Root, "other")
		if _, err := good(ctx, d, other); err != nil {
			return store.PullStats{}, err
		}
		if err := os.Rename(other, m.Dir(d)); err != nil {
			return store.PullStats{}, err
		}
		return good(ctx, d, dest)
	}
	dir, err := m.Ensure(context.Background(), f.a)
	if err != nil {
		t.Fatal(err)
	}
	sameTree(t, f.srcA, dir)
	if ents, _ := os.ReadDir(filepath.Join(m.Root, "tmp")); len(ents) != 0 {
		t.Fatalf("tmp holds %d entries", len(ents))
	}
}

// Init removes pulls a crashed process left in tmp/.
func TestInitRemovesUnfinishedPulls(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "tmp", "abc-123"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := &Materializer{Root: root}
	if err := m.Init(); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(filepath.Join(root, "tmp")); len(ents) != 0 {
		t.Fatalf("tmp holds %d entries", len(ents))
	}
}

// Leases count the volumes per artifact; releasing them never deletes the artifact.
func TestLeases(t *testing.T) {
	f := newFixture(t)
	m, _ := newMaterializer(t, f)
	dir, err := m.Ensure(context.Background(), f.a)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"csi-1", "csi-2", "csi-2", "weird/../id"} { // csi-2 twice: idempotent
		if err := m.Acquire(v, f.a, "/pods/"+v); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := m.Users(f.a); n != 3 {
		t.Fatalf("%d users, want 3", n)
	}
	for i, v := range []string{"csi-1", "weird/../id", "csi-2"} {
		d, n, ok, err := m.Release(v)
		if err != nil || !ok || d != f.a || n != 2-i {
			t.Fatalf("release %s: %v %d %v %v", v, d, n, ok, err)
		}
	}
	if _, _, ok, err := m.Release("csi-1"); ok || err != nil {
		t.Fatalf("second release: ok %v err %v", ok, err)
	}
	if !isDir(dir) {
		t.Fatal("artifact directory removed")
	}
	sameTree(t, f.srcA, dir)
}
