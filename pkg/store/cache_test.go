package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ai-computing/onggi/pkg/backend"
	bfs "github.com/ai-computing/onggi/pkg/backend/fs"
	"github.com/ai-computing/onggi/pkg/codec"
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
