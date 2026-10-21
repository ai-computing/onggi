package dedup

import (
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/ai-computing/onggi/pkg/chunker"
	"github.com/ai-computing/onggi/pkg/layout"
)

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func random(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

// A small model of what the lab store holds: an HF checkpoint, a byte-identical RNGD copy, and
// two ATOM graphs that embed the same weights at different offsets.
func TestReportOnAStoreWithKnownSharing(t *testing.T) {
	dir := t.TempDir()
	weights := random(1, 6<<20)
	write(t, filepath.Join(dir, "hub/model.safetensors"), weights)
	write(t, filepath.Join(dir, "furiosa/param_files/params-x/model.safetensors"), weights)
	write(t, filepath.Join(dir, "atom/decode_S256_B1.rbln"), append(random(2, 300_001), weights...))
	write(t, filepath.Join(dir, "atom/decode_S256_B2.rbln"), append(random(3, 777_777), weights...))
	write(t, filepath.Join(dir, "atom/prefill_S512.rbln"), random(4, 4<<20))

	files, skipped, err := Collect([]string{dir}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 5 || len(skipped) != 0 {
		t.Fatalf("collected %d files, skipped %v", len(files), skipped)
	}
	rep, err := Run(files, skipped, Options{Params: chunker.Default, Workers: 3, Pairs: 3, FixedBlock: 1 << 20}, nil)
	if err != nil {
		t.Fatal(err)
	}
	by := map[layout.Kind]Group{}
	for _, g := range rep.Groups {
		by[g.Kind] = g
	}
	if by[layout.RNGDParams].NewBytes != 0 {
		t.Errorf("RNGD copy added %d new bytes, want 0", by[layout.RNGDParams].NewBytes)
	}
	// The ATOM decode graphs add only their distinct prefixes (plus a few boundary chunks).
	if got, max := by[layout.ATOMDecode].NewBytes, int64(300_001+777_777+4*chunker.Default.Max); got > max {
		t.Errorf("ATOM decode added %d new bytes, want <= %d", got, max)
	}
	if by[layout.ATOMPrefill].NewBytes != 4<<20 {
		t.Errorf("prefill added %d new bytes, want all of it", by[layout.ATOMPrefill].NewBytes)
	}
	if rep.Ratio < 2.2 {
		t.Errorf("ratio %.2f, want > 2.2", rep.Ratio)
	}
	// Fixed blocks find the identical HF/RNGD copy but not the shifted ATOM weights.
	if rep.Fixed == nil || rep.Fixed.Ratio >= rep.Ratio {
		t.Errorf("fixed-block ratio %+v should be below the content-defined %.2f", rep.Fixed, rep.Ratio)
	}
	found := false
	for _, p := range rep.Pairs {
		if filepath.Base(p.A) == "decode_S256_B1.rbln" && filepath.Base(p.B) == "decode_S256_B2.rbln" {
			found = true
			if p.Share < 0.85 {
				t.Errorf("decode B1 share in B2 %.3f, want > 0.85", p.Share)
			}
		}
	}
	if !found {
		t.Error("pair decode_S256_B1 -> decode_S256_B2 missing")
	}
}

func TestOrderIsSourcesFirst(t *testing.T) {
	dir := t.TempDir()
	w := random(5, 1<<20)
	write(t, filepath.Join(dir, "a/param_files/p/x.safetensors"), w) // sorts before "z/" by path
	write(t, filepath.Join(dir, "z/hub/x.safetensors"), w)
	files, _, err := Collect([]string{dir}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if files[0].Kind != layout.HF {
		t.Fatalf("first file is %s, want the HF source", files[0].Kind)
	}
}

func TestSymlinksAreSkipped(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "blobs/abc"), random(6, 1000))
	if err := os.Symlink(filepath.Join(dir, "blobs/abc"), filepath.Join(dir, "link.safetensors")); err != nil {
		t.Skip("symlinks not available:", err)
	}
	files, skipped, err := Collect([]string{dir}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || len(skipped) != 1 {
		t.Fatalf("files %d skipped %d, want 1 and 1", len(files), len(skipped))
	}
}
