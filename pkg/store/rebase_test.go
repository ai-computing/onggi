package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ai-computing/onggi/pkg/digest"
)

// rebaseFixture: a base artifact pulled into a directory, and a source tree for the new one.
func rebaseFixture(t *testing.T, s *Store, base, next map[string][]byte) (Base, digest.Digest, string) {
	t.Helper()
	ctx := context.Background()
	bm := pushDir(t, s, base)
	dir := t.TempDir()
	if _, err := s.Pull(ctx, bm, dir, PullOptions{}); err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	for n, b := range next {
		write(t, filepath.Join(src, n), b)
	}
	nm, _, err := s.Push(ctx, src, PushOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return Base{Manifest: bm, Dir: dir}, nm, src
}

func TestRebaseWritesOnlyWhatChanged(t *testing.T) {
	ctx := context.Background()
	s := openTwoLevel(t, t.TempDir())
	w := random(31, 8<<20)
	v := append([]byte(nil), w...)
	copy(v[3<<20:], random(32, 200<<10)) // new bytes in place
	copy(v[5<<20:], w[:256<<10])         // bytes the base has elsewhere: their chunks now go to two places
	b, nm, src := rebaseFixture(t, s,
		map[string][]byte{"decode_B2.rbln": w, "config.json": []byte(`{"batch": 2}`)},
		map[string][]byte{"decode_B8.rbln": v, "config.json": []byte(`{"batch": 8}`)})

	claimed := 0
	out := t.TempDir()
	st, err := s.Pull(ctx, nm, out, PullOptions{Bases: []Base{b}, Claim: func(x Base) (string, bool) {
		claimed++
		return x.Dir, true
	}})
	if err != nil {
		t.Fatal(err)
	}
	sameTree(t, src, out)
	t.Logf("%+v", st)
	if claimed != 1 || st.Base != b.Manifest.String() || st.Reused < int64(len(v))*9/10 || st.ReadBytes > int64(len(v))/10 ||
		st.BaseBest < st.Reused {
		t.Fatalf("rebase: claimed %d, %+v", claimed, st)
	}
	if _, err := os.Stat(filepath.Join(b.Dir, "decode_B2.rbln")); !os.IsNotExist(err) {
		t.Fatalf("base file still in the base directory: %v", err)
	}
}

func TestRebaseNeedsEnoughInPlace(t *testing.T) {
	ctx := context.Background()
	s := openTwoLevel(t, t.TempDir())
	b, nm, src := rebaseFixture(t, s,
		map[string][]byte{"decode_B2.rbln": random(41, 4<<20)},
		map[string][]byte{"decode_B8.rbln": random(42, 4<<20)})
	out := t.TempDir()
	st, err := s.Pull(ctx, nm, out, PullOptions{Bases: []Base{b}, Claim: func(Base) (string, bool) {
		t.Fatal("claimed a base that keeps nothing")
		return "", false
	}})
	if err != nil {
		t.Fatal(err)
	}
	sameTree(t, src, out)
	if st.Base != "" || st.Reused != 0 || st.BaseBest >= 2<<20 { // the best keeps less than half
		t.Fatalf("%+v", st)
	}
}

func TestRebaseClaimRefused(t *testing.T) {
	ctx := context.Background()
	s := openTwoLevel(t, t.TempDir())
	w := random(51, 4<<20)
	v := append([]byte(nil), w...)
	copy(v[1<<20:], random(52, 64<<10))
	b, nm, src := rebaseFixture(t, s, map[string][]byte{"m.rbln": w}, map[string][]byte{"m.rbln": v})
	out := t.TempDir()
	st, err := s.Pull(ctx, nm, out, PullOptions{Bases: []Base{b}, Claim: func(Base) (string, bool) { return "", false }})
	if err != nil {
		t.Fatal(err)
	}
	sameTree(t, src, out)
	if st.Base != "" || st.Reused != 0 {
		t.Fatalf("%+v", st)
	}
	if _, err := os.Stat(filepath.Join(b.Dir, "m.rbln")); err != nil {
		t.Fatalf("refused base was touched: %v", err)
	}
}

// A base whose manifest the store no longer has is skipped.
func TestRebaseSkipsUnknownBase(t *testing.T) {
	ctx := context.Background()
	s := openTwoLevel(t, t.TempDir())
	w := random(61, 2<<20)
	_, nm, src := rebaseFixture(t, s, map[string][]byte{"m.rbln": w}, map[string][]byte{"m.rbln": w})
	out := t.TempDir()
	st, err := s.Pull(ctx, nm, out, PullOptions{Bases: []Base{{Manifest: digest.Of([]byte("gone")), Dir: t.TempDir()}},
		Claim: func(Base) (string, bool) { t.Fatal("claimed an unknown base"); return "", false }})
	if err != nil {
		t.Fatal(err)
	}
	sameTree(t, src, out)
	if st.Base != "" {
		t.Fatalf("%+v", st)
	}
}

// After delta apply only a few chunks are left, and the node cache finds them by scanning its
// pack indexes instead of loading its whole index (#42).
func TestRebaseThroughCacheReadsTheRestFromIt(t *testing.T) {
	ctx := context.Background()
	s := openTwoLevel(t, t.TempDir())
	cacheDir := t.TempDir()
	w := random(71, 16<<20)
	patch := random(72, 384<<10) // large next to the chunks at its edges, which are new
	v := append([]byte(nil), w...)
	copy(v[5<<20:], patch)
	b, nm, src := rebaseFixture(t, s, map[string][]byte{"m.rbln": w}, map[string][]byte{"m.rbln": v})
	// the cache has seen the patch bytes in another artifact, and the base's chunks
	other := pushDir(t, s, map[string][]byte{"p.rbln": patch})
	for _, d := range []digest.Digest{b.Manifest, other} {
		if _, err := s.Pull(ctx, d, t.TempDir(), PullOptions{Cache: openCache(t, cacheDir, s)}); err != nil {
			t.Fatal(err)
		}
	}
	c := openCache(t, cacheDir, s)
	out := t.TempDir()
	st, err := s.Pull(ctx, nm, out, PullOptions{Cache: c, Bases: []Base{b}, Claim: func(x Base) (string, bool) { return x.Dir, true }})
	if err != nil {
		t.Fatal(err)
	}
	sameTree(t, src, out)
	t.Logf("%+v", st)
	if st.Base == "" || st.CacheBytes == 0 || st.ReadBytes > st.CacheBytes {
		t.Fatalf("%+v", st)
	}
	if c.loaded {
		t.Fatal("the cache's whole index was loaded for a few chunks")
	}
}

// Shifted content, as when a TensorRT engine's plan grows by 17,952 bytes in front of the
// weights: nothing stays at the same offset, so the base is not consumed, but its files serve
// the chunks where they are (#58).
func TestBaseServesShiftedContent(t *testing.T) {
	ctx := context.Background()
	s := openTwoLevel(t, t.TempDir())
	w := random(91, 8<<20)
	v := append(random(92, 17952), w...)
	b, nm, src := rebaseFixture(t, s, map[string][]byte{"rank0.engine": w}, map[string][]byte{"rank0.engine": v})
	out := t.TempDir()
	st, err := s.Pull(ctx, nm, out, PullOptions{Bases: []Base{b}, Claim: func(Base) (string, bool) {
		t.Fatal("consumed a base that keeps nothing in place")
		return "", false
	}})
	if err != nil {
		t.Fatal(err)
	}
	sameTree(t, src, out)
	t.Logf("%+v", st)
	if st.LocalBase != b.Manifest.String() || st.LocalBytes < int64(len(w))*9/10 || st.ReadBytes > int64(len(v))/10 {
		t.Fatalf("shifted content not read from the base: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(b.Dir, "rank0.engine")); err != nil {
		t.Fatalf("the base was touched: %v", err)
	}
}
