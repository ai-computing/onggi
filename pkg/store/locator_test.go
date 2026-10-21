package store

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ai-computing/onggi/pkg/backend"
	"github.com/ai-computing/onggi/pkg/codec"
	"github.com/ai-computing/onggi/pkg/digest"
)

// withoutLocator stores a copy of manifest md with no locator, as onggi wrote before #17, and
// returns its digest.
func withoutLocator(t *testing.T, s *Store, md digest.Digest) digest.Digest {
	t.Helper()
	m, err := s.ReadManifest(context.Background(), md)
	if err != nil {
		t.Fatal(err)
	}
	m.Locator = nil
	data, _ := json.MarshalIndent(m, "", " ")
	d := digest.Of(data)
	if err := s.B.Put(context.Background(), backend.Key{Kind: backend.Manifest, Name: d.Hex()}, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	return d
}

func twoLevelSource(t *testing.T) string {
	src := t.TempDir()
	w := weights(21, 2<<20)
	write(t, filepath.Join(src, "a.rbln"), w)
	write(t, filepath.Join(src, "b.rbln"), shuffled(w, 22))
	write(t, filepath.Join(src, "same.rbln"), w)
	write(t, filepath.Join(src, "tail.bin"), random(23, 70_001))
	write(t, filepath.Join(src, "empty"), nil)
	return src
}

// A pull with a locator reads neither the index nor the chunk lists.
func TestPullWithLocatorNeedsNoIndex(t *testing.T) {
	ctx := context.Background()
	src, storeDir := twoLevelSource(t), t.TempDir()
	s := openTwoLevel(t, storeDir)
	md, _, err := s.Push(ctx, src, PushOptions{Compress: codec.ModeAuto})
	if err != nil {
		t.Fatal(err)
	}
	m, _ := s.ReadManifest(ctx, md)
	if m.Locator == nil {
		t.Fatal("push wrote no locator")
	}
	for _, dir := range []string{"index", "lists"} {
		filepath.WalkDir(filepath.Join(storeDir, dir), func(p string, d os.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() && filepath.Base(p) != m.Locator.Hex() {
				os.Chmod(p, 0o644)
				os.Remove(p)
			}
			return nil
		})
	}
	out := t.TempDir()
	if _, err := openTwoLevel(t, storeDir).Pull(ctx, md, out, PullOptions{Workers: 3, MaxRange: 128 << 10}); err != nil {
		t.Fatal(err)
	}
	sameTree(t, src, out)
}

// Manifests without a locator still pull through the index.
func TestPullWithoutLocatorUsesTheIndex(t *testing.T) {
	ctx := context.Background()
	src, storeDir := twoLevelSource(t), t.TempDir()
	s := openTwoLevel(t, storeDir)
	md, _, err := s.Push(ctx, src, PushOptions{Compress: codec.ModeAuto})
	if err != nil {
		t.Fatal(err)
	}
	old := withoutLocator(t, s, md)
	out := t.TempDir()
	if _, err := openTwoLevel(t, storeDir).Pull(ctx, old, out, PullOptions{}); err != nil {
		t.Fatal(err)
	}
	sameTree(t, src, out)
}

func TestLocatorRejectsDamage(t *testing.T) {
	ctx := context.Background()
	src, storeDir := twoLevelSource(t), t.TempDir()
	s := openTwoLevel(t, storeDir)
	md, _, err := s.Push(ctx, src, PushOptions{})
	if err != nil {
		t.Fatal(err)
	}
	m, _ := s.ReadManifest(ctx, md)
	data, err := readAll(ctx, s.B, backend.Key{Kind: backend.List, Name: m.Locator.Hex()})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := readLocator(data, m); err != nil {
		t.Fatalf("intact locator: %v", err)
	}
	for _, bad := range [][]byte{data[:len(data)-3], append(append([]byte(nil), data...), 0), data[:20]} {
		if _, _, err := readLocator(bad, m); err == nil {
			t.Errorf("accepted a damaged locator of %d bytes", len(bad))
		}
	}
	// a locator whose bytes changed fails its digest check before it is decoded
	p := filepath.Join(storeDir, "lists", m.Locator.Hex()[:2], m.Locator.Hex())
	if _, err := os.Stat(p); err != nil {
		filepath.WalkDir(filepath.Join(storeDir, "lists"), func(q string, d os.DirEntry, err error) error {
			if err == nil && filepath.Base(q) == m.Locator.Hex() {
				p = q
			}
			return nil
		})
	}
	data[len(data)/2] ^= 1
	os.Chmod(p, 0o644)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openTwoLevel(t, storeDir).Pull(ctx, md, t.TempDir(), PullOptions{}); err == nil {
		t.Fatal("pull accepted a changed locator")
	}
}
