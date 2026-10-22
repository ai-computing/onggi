package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ai-computing/onggi/pkg/backend"
	bfs "github.com/ai-computing/onggi/pkg/backend/fs"
	"github.com/ai-computing/onggi/pkg/codec"
	"github.com/ai-computing/onggi/pkg/digest"
)

func openCache(t *testing.T, dir string, like *Store) *Store {
	t.Helper()
	b, err := bfs.Open(dir, backend.DurabilityNone)
	if err != nil {
		t.Fatal(err)
	}
	c, err := OpenWith(context.Background(), b, Create{Chunker: like.Config.Chunker, Sub: like.Config.Sub, PackSize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A node that pulled one variant reads only what the second variant adds; a repeat pull reads
// nothing from the store.
func TestNodeCacheReadsOnlyWhatIsMissing(t *testing.T) {
	ctx := context.Background()
	w := weights(31, 2<<20)
	a, b := t.TempDir(), t.TempDir()
	write(t, filepath.Join(a, "decode_B1.rbln"), w)
	write(t, filepath.Join(b, "decode_B8.rbln"), append(shuffled(w, 32), random(33, 100_000)...))
	storeDir, cacheDir := t.TempDir(), t.TempDir()
	s := openTwoLevel(t, storeDir)
	ma, _, err := s.Push(ctx, a, PushOptions{Compress: codec.ModeAuto})
	if err != nil {
		t.Fatal(err)
	}
	mb, stb, err := s.Push(ctx, b, PushOptions{Compress: codec.ModeAuto})
	if err != nil {
		t.Fatal(err)
	}

	remote := openTwoLevel(t, storeDir)
	cache := openCache(t, cacheDir, remote)
	outA, outB, outA2 := t.TempDir(), t.TempDir(), t.TempDir()
	pa, err := remote.Pull(ctx, ma, outA, PullOptions{Cache: cache, Workers: 3})
	if err != nil {
		t.Fatal(err)
	}
	sameTree(t, a, outA)
	if pa.CacheBytes != 0 || pa.Cached != pa.ReadBytes || pa.ReadBytes == 0 {
		t.Fatalf("first pull: store %d, cache %d, cached %d", pa.ReadBytes, pa.CacheBytes, pa.Cached)
	}

	// a fresh process: the cache store is reopened from disk
	cache = openCache(t, cacheDir, remote)
	pb, err := remote.Pull(ctx, mb, outB, PullOptions{Cache: cache, Workers: 3})
	if err != nil {
		t.Fatal(err)
	}
	sameTree(t, b, outB)
	t.Logf("second variant: store %d B (its new chunks were %d B stored), cache %d B", pb.ReadBytes, stb.PackBytes, pb.CacheBytes)
	if pb.ReadBytes > stb.PackBytes || pb.CacheBytes == 0 {
		t.Fatalf("second pull read %d B from the store; the variant added only %d B", pb.ReadBytes, stb.PackBytes)
	}

	pa2, err := remote.Pull(ctx, ma, outA2, PullOptions{Cache: openCache(t, cacheDir, remote)})
	if err != nil {
		t.Fatal(err)
	}
	sameTree(t, a, outA2)
	if pa2.ReadBytes != 0 || pa2.Cached != 0 {
		t.Fatalf("repeat pull read %d B from the store and cached %d B", pa2.ReadBytes, pa2.Cached)
	}
}

// Chunks read from the cache are hash-checked like any other.
func TestNodeCacheCorruptionIsDetected(t *testing.T) {
	ctx := context.Background()
	a := t.TempDir()
	write(t, filepath.Join(a, "w.rbln"), weights(34, 1<<20))
	storeDir, cacheDir := t.TempDir(), t.TempDir()
	s := openTwoLevel(t, storeDir)
	ma, _, err := s.Push(ctx, a, PushOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pull(ctx, ma, t.TempDir(), PullOptions{Cache: openCache(t, cacheDir, s)}); err != nil {
		t.Fatal(err)
	}
	filepath.WalkDir(filepath.Join(cacheDir, "packs"), func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			data, _ := os.ReadFile(p)
			data[len(data)/2] ^= 0xff
			os.Chmod(p, 0o644)
			os.WriteFile(p, data, 0o644)
		}
		return nil
	})
	out := t.TempDir()
	if _, err := s.Pull(ctx, ma, out, PullOptions{Cache: openCache(t, cacheDir, s)}); err == nil {
		t.Fatal("pull accepted a corrupt cache chunk")
	}
	if _, serr := os.Stat(filepath.Join(out, "w.rbln")); serr == nil {
		t.Fatal("a corrupt file was published")
	}
}

// A cache keeps what recently used artifacts need and evicts the rest (#31).
func TestNodeCacheEviction(t *testing.T) {
	ctx := context.Background()
	w := weights(61, 2<<20)
	a, b := t.TempDir(), t.TempDir()
	write(t, filepath.Join(a, "decode_B1.rbln"), w)
	write(t, filepath.Join(b, "decode_B8.rbln"), append(shuffled(w, 62), random(63, 2<<20)...))
	storeDir, cacheDir := t.TempDir(), t.TempDir()
	s := openTwoLevel(t, storeDir)
	ma, _, err := s.Push(ctx, a, PushOptions{Compress: codec.ModeAuto})
	if err != nil {
		t.Fatal(err)
	}
	mb, stb, err := s.Push(ctx, b, PushOptions{Compress: codec.ModeAuto})
	if err != nil {
		t.Fatal(err)
	}
	remote := openTwoLevel(t, storeDir)
	for _, m := range []digest.Digest{ma, mb} {
		if _, err := remote.Pull(ctx, m, t.TempDir(), PullOptions{Cache: openCache(t, cacheDir, remote)}); err != nil {
			t.Fatal(err)
		}
	}
	full := dirBytes(t, cacheDir, "packs")
	t0 := time.Now()
	// b was last used two days ago; a is current
	if err := openCache(t, cacheDir, remote).recordUse(ctx, mb, t0.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	gc := func(after time.Duration) GCStats {
		c := openCache(t, cacheDir, remote)
		st, err := c.GC(ctx, nil, GCOptions{RootStore: openTwoLevel(t, storeDir), KeepUsed: 24 * time.Hour,
			Grace: time.Hour, LeaseTTL: 1000 * time.Hour, RepackDead: 0.25, Now: clock(t0, after)})
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	st1 := gc(0)
	if st1.Roots != 1 || st1.LiveManifests != 1 {
		t.Fatalf("cache GC roots: %+v", st1)
	}
	gc(2 * time.Hour)
	gc(4 * time.Hour)
	after := dirBytes(t, cacheDir, "packs")
	t.Logf("cache packs %d B -> %d B (b added %d B stored)", full, after, stb.PackBytes)
	if after >= full {
		t.Fatalf("cache did not shrink: %d -> %d", full, after)
	}
	// a is still fully cached; b reads back only what it adds over a
	pa, err := remote.Pull(ctx, ma, t.TempDir(), PullOptions{Cache: openCache(t, cacheDir, remote)})
	if err != nil || pa.ReadBytes != 0 {
		t.Fatalf("pull of a after eviction: %d B from the store, %v", pa.ReadBytes, err)
	}
	out := t.TempDir()
	pb, err := remote.Pull(ctx, mb, out, PullOptions{Cache: openCache(t, cacheDir, remote)})
	if err != nil {
		t.Fatal(err)
	}
	sameTree(t, b, out)
	if pb.ReadBytes == 0 || pb.ReadBytes > stb.PackBytes {
		t.Fatalf("pull of b after eviction read %d B from the store; b adds %d B", pb.ReadBytes, stb.PackBytes)
	}
}

// A root without a locator stops the cache GC before it deletes anything.
func TestNodeCacheGCNeedsLocators(t *testing.T) {
	ctx := context.Background()
	a := t.TempDir()
	write(t, filepath.Join(a, "w.bin"), random(64, 1<<20))
	storeDir, cacheDir := t.TempDir(), t.TempDir()
	s := openTwoLevel(t, storeDir)
	ma, _, err := s.Push(ctx, a, PushOptions{})
	if err != nil {
		t.Fatal(err)
	}
	old := withoutLocator(t, s, ma)
	c := openCache(t, cacheDir, s)
	if _, err := s.Pull(ctx, old, t.TempDir(), PullOptions{Cache: c}); err != nil {
		t.Fatal(err)
	}
	if _, err := openCache(t, cacheDir, s).GC(ctx, nil, GCOptions{RootStore: s, KeepUsed: time.Hour}); err == nil {
		t.Fatal("cache GC ran with a root it cannot mark")
	}
}
