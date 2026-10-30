package store

import (
	"bytes"
	"context"
	"io"
	"reflect"
	"path/filepath"
	"testing"

	"github.com/ai-computing/onggi/pkg/backend"
	"github.com/ai-computing/onggi/pkg/digest"
)

type diffReads struct {
	backend.Backend
	gets map[backend.Key]int
}

func (b *diffReads) Get(ctx context.Context, k backend.Key, off, n int64) (io.ReadCloser, error) {
	b.gets[k]++
	return b.Backend.Get(ctx, k, off, n)
}

func TestDiffReadsEachLocatorOnce(t *testing.T) {
	ctx := context.Background()
	s := openTwoLevel(t, t.TempDir())
	w := random(29, 2<<20)
	base := pushDir(t, s, map[string][]byte{"weights.rbln": w})
	v := append([]byte(nil), w...)
	copy(v[1<<20:], random(30, 64<<10))
	newID := pushDir(t, s, map[string][]byte{"weights.rbln": v})
	bm, err := s.ReadManifest(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	nm, err := s.ReadManifest(ctx, newID)
	if err != nil {
		t.Fatal(err)
	}
	if bm.Locator == nil || nm.Locator == nil || *bm.Locator == *nm.Locator {
		t.Fatal("fixture needs two distinct locators")
	}
	b := &diffReads{Backend: s.B, gets: map[backend.Key]int{}}
	s.B = b
	st, err := s.Diff(ctx, base, newID, DiffOptions{Ranges: true})
	if err != nil {
		t.Fatal(err)
	}
	if st.Same+st.Moved+st.New != st.Size || st.NewStored == 0 || st.ChangedRuns == 0 {
		t.Fatalf("invalid diff: %+v", st)
	}
	for _, d := range []*digest.Digest{bm.Locator, nm.Locator} {
		key := backend.Key{Kind: backend.List, Name: d.Hex()}
		if got := b.gets[key]; got != 1 {
			t.Errorf("locator read %d times, want 1", got)
		}
	}
}

func TestDiffLegacyAndIdenticalManifests(t *testing.T) {
	ctx := context.Background()
	s := openTwoLevel(t, t.TempDir())
	w := random(31, 2<<20)
	base := pushDir(t, s, map[string][]byte{"weights.rbln": w, "empty": nil})
	v := append([]byte(nil), w...)
	copy(v[1<<20:], random(32, 64<<10))
	newID := pushDir(t, s, map[string][]byte{"weights.rbln": v, "empty": nil})
	want, err := s.Diff(ctx, base, newID, DiffOptions{Ranges: true})
	if err != nil {
		t.Fatal(err)
	}
	legacyBase, legacyNew := withoutLocator(t, s, base), withoutLocator(t, s, newID)
	for _, pair := range [][2]digest.Digest{{legacyBase, newID}, {base, legacyNew}, {legacyBase, legacyNew}} {
		got, err := s.Diff(ctx, pair[0], pair[1], DiffOptions{Ranges: true})
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("legacy diff differs: %+v, %v; want %+v", got, err, want)
		}
	}
	for _, id := range []digest.Digest{base, legacyBase} {
		got, err := s.Diff(ctx, id, id, DiffOptions{Ranges: true})
		if err != nil || got.Same != got.Size || got.NewStored != 0 || got.ChangedRuns != 0 {
			t.Fatalf("identical diff: %+v, %v", got, err)
		}
	}
}

func pushDir(t *testing.T, s *Store, files map[string][]byte) digest.Digest {
	t.Helper()
	src := t.TempDir()
	for n, b := range files {
		write(t, filepath.Join(src, n), b)
	}
	m, _, err := s.Push(context.Background(), src, PushOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestDiff(t *testing.T) {
	ctx := context.Background()
	s := openTwoLevel(t, t.TempDir())
	w := random(21, 8<<20)
	base := pushDir(t, s, map[string][]byte{"decode_B2.rbln": w, "config.json": []byte(`{"batch": 2}`)})

	// identical content under another name: all Same, paired by kind
	st, err := s.Diff(ctx, base, pushDir(t, s, map[string][]byte{"decode_B8.rbln": w, "config.json": []byte(`{"batch": 2}`)}), DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Same != st.Size || st.SameBlocks != st.Size || st.New != 0 || st.ChangedRuns != 0 || st.NewStored != 0 {
		t.Fatalf("identical: %+v", st)
	}
	if st.Files[0].Base == "" || st.Files[1].Base == "" {
		t.Fatalf("not paired: %+v", st.Files)
	}

	// one region overwritten in place
	v := append([]byte(nil), w...)
	copy(v[3<<20:], random(22, 200<<10))
	st, err = s.Diff(ctx, base, pushDir(t, s, map[string][]byte{"decode_B2.rbln": v}), DiffOptions{Ranges: true})
	if err != nil {
		t.Fatal(err)
	}
	f := st.Files[0]
	t.Logf("in place: %+v", f)
	// sub-chunks next to the region may resynchronize and split it into a few runs
	if f.Same+f.Moved+f.New != f.Size || f.New < 200<<10 || f.New > 600<<10 || f.ChangedRuns < 1 || f.ChangedRuns > 3 ||
		f.LargestRun < 200<<10 || f.SameBlocks > f.Same {
		t.Fatalf("in place: %+v", f)
	}
	if st.NewStored == 0 {
		t.Fatalf("no new stored bytes: %+v", st)
	}

	// the ranges cover exactly the bytes that are not at the same offset, in order (#46)
	var covered, end int64
	for _, r := range f.Changed {
		if r[0] < end || r[1] <= 0 || r[0]+r[1] > f.Size {
			t.Fatalf("ranges out of order or out of the file: %v", f.Changed)
		}
		covered, end = covered+r[1], r[0]+r[1]
	}
	if len(f.Changed) != f.ChangedRuns || covered != f.Size-f.Same || int64(len(v)) != f.Size {
		t.Fatalf("ranges %v cover %d B, want %d", f.Changed, covered, f.Size-f.Same)
	}
	if !bytes.Equal(v[:f.Changed[0][0]], w[:f.Changed[0][0]]) {
		t.Fatal("bytes before the first changed range differ from the base")
	}

	// the same bytes rearranged in 32 KiB blocks: most found elsewhere in the base, little at the
	// same offset; sub-chunks that straddle block boundaries are new
	st, err = s.Diff(ctx, base, pushDir(t, s, map[string][]byte{"decode_B2.rbln": shuffled(w, 23)}), DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	f = st.Files[0]
	t.Logf("rearranged: %+v", f)
	if f.Same+f.Moved+f.New != f.Size || f.Moved < f.Size/2 || f.Same > f.Size/10 || f.Changed != nil {
		t.Fatalf("rearranged: %+v", f)
	}
}
