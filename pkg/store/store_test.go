package store

import (
	"bytes"
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ai-computing/onggi/pkg/backend"
	bfs "github.com/ai-computing/onggi/pkg/backend/fs"
	"github.com/ai-computing/onggi/pkg/chunker"
)

func random(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func write(t *testing.T, p string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o640); err != nil {
		t.Fatal(err)
	}
}

func openStore(t *testing.T, root string) *Store {
	t.Helper()
	b, err := bfs.Open(root, backend.DurabilityNone)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), b, chunker.Default, 1<<20) // small packs to exercise many of them
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func storedBytes(t *testing.T, root string) int64 {
	var n int64
	filepath.WalkDir(filepath.Join(root, "packs"), func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			info, _ := d.Info()
			n += info.Size()
		}
		return nil
	})
	return n
}

func sameTree(t *testing.T, a, b string) {
	t.Helper()
	filepath.WalkDir(a, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(a, p)
		x, _ := os.ReadFile(p)
		y, err := os.ReadFile(filepath.Join(b, rel))
		if err != nil {
			t.Fatalf("%s missing after pull: %v", rel, err)
		}
		if !bytes.Equal(x, y) {
			t.Fatalf("%s differs after pull", rel)
		}
		return nil
	})
}

// The lab case in small: two "ATOM variants" holding the same weights at different offsets, an
// "RNGD copy" equal to the source, and an empty file.
func TestPushPullRoundTripWithDedup(t *testing.T) {
	ctx := context.Background()
	src, storeDir, out := t.TempDir(), t.TempDir(), t.TempDir()
	w := random(1, 5<<20)
	write(t, filepath.Join(src, "hub/model.safetensors"), w)
	write(t, filepath.Join(src, "param_files/model.safetensors"), w)
	write(t, filepath.Join(src, "atom/decode_S256_B1.rbln"), append(random(2, 300_001), w...))
	write(t, filepath.Join(src, "atom/decode_S512_B4.rbln"), append(random(3, 77_777), w...))
	write(t, filepath.Join(src, "atom/empty"), nil)

	s := openStore(t, storeDir)
	md, st, err := s.Push(ctx, src, PushOptions{Workers: 3})
	if err != nil {
		t.Fatal(err)
	}
	logical := int64(4*len(w) + 300_001 + 77_777)
	if st.Bytes != logical || st.Files != 5 {
		t.Fatalf("push read %d bytes in %d files, want %d in 5", st.Bytes, st.Files, logical)
	}
	if stored := storedBytes(t, storeDir); stored > int64(len(w))+300_001+77_777+4*int64(chunker.Default.Max) {
		t.Fatalf("stored %d bytes for %d logical; dedup failed", stored, logical)
	}

	// a fresh process sees the same store and pulls it back
	s2 := openStore(t, storeDir)
	if _, err := s2.Pull(ctx, md, out, PullOptions{Workers: 4, MaxRange: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	sameTree(t, src, out)

	// pushing again uploads nothing new
	_, st2, err := s2.Push(ctx, src, PushOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if st2.NewBytes != 0 || st2.Packs != 0 {
		t.Fatalf("second push wrote %d new bytes in %d packs, want none", st2.NewBytes, st2.Packs)
	}
}

func TestPullDetectsCorruptPack(t *testing.T) {
	ctx := context.Background()
	src, storeDir := t.TempDir(), t.TempDir()
	write(t, filepath.Join(src, "a.bin"), random(4, 3<<20))
	s := openStore(t, storeDir)
	md, _, err := s.Push(ctx, src, PushOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var pack string
	filepath.WalkDir(filepath.Join(storeDir, "packs"), func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() && pack == "" {
			pack = p
		}
		return nil
	})
	data, _ := os.ReadFile(pack)
	data[len(data)/2] ^= 0xff
	os.Chmod(pack, 0o644)
	if err := os.WriteFile(pack, data, 0o644); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	_, err = openStore(t, storeDir).Pull(ctx, md, out, PullOptions{})
	if err == nil || !strings.Contains(err.Error(), "hash check") {
		t.Fatalf("pull of a corrupt pack: %v, want a hash check failure", err)
	}
	if _, serr := os.Stat(filepath.Join(out, "a.bin")); serr == nil {
		t.Fatal("a corrupt file was published under its final name")
	}
}

func TestStoreKeepsItsChunkParams(t *testing.T) {
	dir := t.TempDir()
	b, _ := bfs.Open(dir, backend.DurabilityNone)
	if _, err := Open(context.Background(), b, chunker.Default, 0); err != nil {
		t.Fatal(err)
	}
	other := chunker.Params{Min: 64 << 10, Avg: 256 << 10, Max: 1 << 20}
	s, err := Open(context.Background(), b, other, 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.Config.Chunker != chunker.Default {
		t.Fatalf("reopened store uses %+v, want the stored %+v", s.Config.Chunker, chunker.Default)
	}
}

func TestSafeJoin(t *testing.T) {
	for _, bad := range []string{"../x", "/etc/passwd", "a/../../x", `a\b`, ""} {
		if _, err := safeJoin("/dest", bad); err == nil {
			t.Errorf("safeJoin accepted %q", bad)
		}
	}
	if _, err := safeJoin("/dest", "atom/decode.rbln"); err != nil {
		t.Fatal(err)
	}
}
