package dedup

import (
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/ai-computing/onggi/pkg/chunker"
)

// rearrange returns the 32 KiB blocks of data in another order: the same bytes in runs shorter
// than a coarse chunk, as between two ATOM prefill variants.
func rearrange(data []byte, seed int64) []byte {
	const blk = 32 << 10
	idx := rand.New(rand.NewSource(seed)).Perm(len(data) / blk)
	out := make([]byte, 0, len(data))
	for _, i := range idx {
		out = append(out, data[i*blk:(i+1)*blk]...)
	}
	return out
}

func classify(t *testing.T, refs []string, paths ...string) map[string]File {
	t.Helper()
	files, skipped, err := Collect(paths, 0)
	if err != nil {
		t.Fatal(err)
	}
	c := &Classify{Fine: DefaultFine, References: refs, RefShare: DefaultRefShare, RearrangedGap: DefaultRearrangeGap,
		ShiftedShare: DefaultShiftedShare}
	rep, err := Run(files, skipped, Options{Params: chunker.Default, Workers: 3, Classify: c}, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]File{}
	for _, f := range rep.Files {
		out[filepath.Base(f.Path)] = f
		t.Logf("%-16s %-20s ref %.3f coarse %.3f fine %.3f", filepath.Base(f.Path), f.Class, f.Shares.Ref, f.Shares.Coarse, f.Shares.Fine)
	}
	return out
}

// The three placement classes and "new", from bytes alone: no name or format is read.
func TestPlacementClasses(t *testing.T) {
	dir := t.TempDir()
	w := random(31, 6<<20)
	write(t, filepath.Join(dir, "a.bin"), append(random(32, 300_001), w...))
	write(t, filepath.Join(dir, "b.bin"), append(random(33, 777_777), w...))
	write(t, filepath.Join(dir, "c.bin"), rearrange(w, 34))
	write(t, filepath.Join(dir, "d.bin"), random(35, 2<<20))
	got := classify(t, nil, dir)
	for name, want := range map[string]string{"a.bin": EmbeddedShifted, "b.bin": EmbeddedShifted, "c.bin": EmbeddedRearranged, "d.bin": New} {
		if got[name].Class != want {
			t.Errorf("%s: class %q, want %q", name, got[name].Class, want)
		}
	}
	if s := got["c.bin"].Shares; s.Coarse > 0.2 || s.Fine < 0.6 {
		t.Errorf("rearranged file: coarse %.3f, fine %.3f", s.Coarse, s.Fine)
	}
}

// A file that holds the reference bytes behind a different header is referenced; the reference
// may be a link, as in a model hub snapshot.
func TestPlacementReferenced(t *testing.T) {
	dir, ref := t.TempDir(), t.TempDir()
	w := random(41, 4<<20)
	write(t, filepath.Join(ref, "blobs", "x"), append([]byte("header of the checkpoint"), w...))
	if err := os.Symlink(filepath.Join(ref, "blobs", "x"), filepath.Join(ref, "model.safetensors")); err != nil {
		t.Skip("no symlinks:", err)
	}
	write(t, filepath.Join(dir, "copy.bin"), append([]byte("another header, 16"), w...))
	write(t, filepath.Join(dir, "other.bin"), random(42, 1<<20))
	got := classify(t, []string{filepath.Join(ref, "model.safetensors")}, dir)
	if got["copy.bin"].Class != Referenced || got["other.bin"].Class != New {
		t.Fatalf("copy %q, other %q", got["copy.bin"].Class, got["other.bin"].Class)
	}
}

func TestClassRulesAndThresholds(t *testing.T) {
	c := &Classify{RefShare: DefaultRefShare, RearrangedGap: DefaultRearrangeGap, ShiftedShare: DefaultShiftedShare}
	for _, tc := range []struct {
		s    Shares
		want string
	}{
		{Shares{Ref: 0.95, Coarse: 0.95, Fine: 0.95}, Referenced},
		{Shares{Ref: 0.94, Coarse: 0.99, Fine: 0.99}, EmbeddedShifted},
		{Shares{Coarse: 0.37, Fine: 0.86}, EmbeddedRearranged}, // two ATOM prefill variants (#15)
		{Shares{Coarse: 0.60, Fine: 0.84}, EmbeddedShifted},    // gap below 0.25
		{Shares{Coarse: 0.10, Fine: 0.30}, New},
		{Shares{Coarse: 0.49, Fine: 0.50}, New},
	} {
		if got := c.Class(tc.s); got != tc.want {
			t.Errorf("%+v: %q, want %q", tc.s, got, tc.want)
		}
	}
}
