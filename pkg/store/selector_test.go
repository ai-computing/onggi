package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ai-computing/onggi/pkg/backend"
	bfs "github.com/ai-computing/onggi/pkg/backend/fs"
	"github.com/ai-computing/onggi/pkg/chunker"
	"github.com/ai-computing/onggi/pkg/codec"
)

// A base damaged after it was pulled is not carried into the new artifact: with VerifyKept the
// chunk that no longer matches is fetched, and only the rest is left in place.
func TestRebaseVerifiesWhatItKeeps(t *testing.T) {
	ctx := context.Background()
	s := openTwoLevel(t, t.TempDir())
	w := random(41, 8<<20)
	v := append([]byte(nil), w...)
	copy(v[3<<20:], random(42, 200<<10))
	b, nm, src := rebaseFixture(t, s,
		map[string][]byte{"decode_B2.rbln": w},
		map[string][]byte{"decode_B8.rbln": v})

	// flip bytes of the base in a region both artifacts share
	f, err := os.OpenFile(filepath.Join(b.Dir, "decode_B2.rbln"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("damaged after the pull"), 6<<20); err != nil {
		t.Fatal(err)
	}
	f.Close()

	claim := func(x Base) (string, bool) { return x.Dir, true }
	out := t.TempDir()
	st, err := s.Pull(ctx, nm, out, PullOptions{Bases: []Base{b}, Claim: claim, VerifyKept: true})
	if err != nil {
		t.Fatal(err)
	}
	sameTree(t, src, out)
	t.Logf("%+v", st)
	if st.Base != b.Manifest.String() || st.Repaired <= 0 || st.Repaired > 1<<20 || st.Verified < int64(len(v))*8/10 {
		t.Fatalf("damaged chunk not repaired, or too much distrusted: %+v", st)
	}
	if st.Reused+st.Repaired > int64(len(v)) || st.Reused != st.Verified {
		t.Fatalf("kept bytes and verified bytes disagree: %+v", st)
	}
}

// Without VerifyKept the pull reads nothing back, as before.
func TestRebaseDoesNotVerifyByDefault(t *testing.T) {
	ctx := context.Background()
	s := openTwoLevel(t, t.TempDir())
	w := random(43, 4<<20)
	v := append([]byte(nil), w...)
	copy(v[1<<20:], random(44, 100<<10))
	b, nm, src := rebaseFixture(t, s, map[string][]byte{"a.rbln": w}, map[string][]byte{"a.rbln": v})
	out := t.TempDir()
	st, err := s.Pull(ctx, nm, out, PullOptions{Bases: []Base{b}, Claim: func(x Base) (string, bool) { return x.Dir, true }})
	if err != nil {
		t.Fatal(err)
	}
	sameTree(t, src, out)
	if st.Verified != 0 || st.Repaired != 0 || st.Reused < int64(len(v))*8/10 {
		t.Fatalf("%+v", st)
	}
}

// With a local store, BaseCopyRemote leaves a shifted base alone and reads the packs; the same
// pull under BaseCopyAlways reads the base. Both produce the artifact.
func TestBaseCopyRemoteSkipsALocalStore(t *testing.T) {
	ctx := context.Background()
	s := openTwoLevel(t, t.TempDir())
	if !s.B.Caps().Local {
		t.Fatal("the fs backend must report itself local")
	}
	w := random(93, 8<<20)
	v := append(random(94, 17952), w...)
	b, nm, src := rebaseFixture(t, s, map[string][]byte{"rank0.engine": w}, map[string][]byte{"rank0.engine": v})
	never := func(Base) (string, bool) {
		t.Fatal("consumed a base that keeps nothing in place")
		return "", false
	}
	for _, c := range []struct {
		name string
		rule BaseCopy
		base bool
	}{{"remote", BaseCopyRemote, false}, {"never", BaseCopyNever, false}, {"always", BaseCopyAlways, true}} {
		out := t.TempDir()
		st, err := s.Pull(ctx, nm, out, PullOptions{Bases: []Base{b}, Claim: never, BaseCopy: c.rule})
		if err != nil {
			t.Fatal(c.name, err)
		}
		sameTree(t, src, out)
		if got := st.LocalBase != ""; got != c.base {
			t.Fatalf("%s: read the base = %v, want %v: %+v", c.name, got, c.base, st)
		}
		if !c.base && st.ReadBytes == 0 {
			t.Fatalf("%s: nothing read from the store: %+v", c.name, st)
		}
	}
	if _, err := os.Stat(filepath.Join(b.Dir, "rank0.engine")); err != nil {
		t.Fatalf("the base was touched: %v", err)
	}
}

// Chunks that follow one another in a file are read together, and each still reaches fn alone.
func TestReadSpansJoinsNeighbours(t *testing.T) {
	data := random(95, 1<<20)
	p := filepath.Join(t.TempDir(), "f")
	write(t, p, data)
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var jobs []rjob
	for off := 0; off < len(data); off += 4096 { // contiguous, then a hole after each 64 KiB
		if off/65536%2 == 1 {
			continue
		}
		jobs = append(jobs, rjob{node: int32(off / 4096), off: int64(off), n: 4096})
	}
	jobs = append(jobs, rjob{node: -1, off: int64(len(data)) - 100, n: 4096}) // runs past the end
	got := make(map[int32]bool)
	bad := 0
	done := make(chan struct{}, 1)
	done <- struct{}{}
	readSpans(context.Background(), []*os.File{f}, jobs, 4, 32<<10, func(j rjob, b []byte) {
		<-done
		defer func() { done <- struct{}{} }()
		if b == nil {
			bad++
			return
		}
		if string(b) != string(data[j.off:j.off+int64(j.n)]) {
			t.Errorf("chunk at %d has the wrong bytes", j.off)
		}
		got[j.node] = true
	})
	if bad != 1 || len(got) != len(jobs)-1 {
		t.Fatalf("read %d of %d chunks, %d failed", len(got), len(jobs), bad)
	}
}

// Reading a base chunk by chunk and in joined ranges gives the same artifact and the same bytes.
func TestBaseRangeDoesNotChangeTheResult(t *testing.T) {
	ctx := context.Background()
	s := openTwoLevel(t, t.TempDir())
	w := random(96, 8<<20)
	v := append(random(97, 17952), w...)
	b, nm, src := rebaseFixture(t, s, map[string][]byte{"rank0.engine": w}, map[string][]byte{"rank0.engine": v})
	never := func(Base) (string, bool) { return "", false }
	var local [2]int64
	for i, r := range []int64{1, 0} {
		out := t.TempDir()
		st, err := s.Pull(ctx, nm, out, PullOptions{Bases: []Base{b}, Claim: never, BaseRange: r})
		if err != nil {
			t.Fatal(err)
		}
		sameTree(t, src, out)
		local[i] = st.LocalBytes
	}
	if local[0] != local[1] || local[0] < int64(len(w))*9/10 {
		t.Fatalf("bytes read from the base: chunk by chunk %d, joined %d", local[0], local[1])
	}
}

// A base given only to read is never claimed; it is held while it is read and released after.
func TestReadBasesAreHeldAndNeverClaimed(t *testing.T) {
	ctx := context.Background()
	s := openTwoLevel(t, t.TempDir())
	w := random(51, 8<<20)
	v := append([]byte(nil), w...)
	copy(v[3<<20:], random(52, 200<<10))
	b, nm, src := rebaseFixture(t, s, map[string][]byte{"decode_B2.rbln": w}, map[string][]byte{"decode_B8.rbln": v})
	held, released := 0, 0
	out := t.TempDir()
	st, err := s.Pull(ctx, nm, out, PullOptions{ReadBases: []Base{b},
		Claim: func(Base) (string, bool) { t.Fatal("claimed a base given only to read"); return "", false },
		Hold: func(x Base) (func(), bool) {
			if x.Manifest != b.Manifest {
				t.Fatalf("held %s", x.Manifest)
			}
			held++
			return func() { released++ }, true
		}})
	if err != nil {
		t.Fatal(err)
	}
	sameTree(t, src, out)
	if held != 1 || released != 1 || st.LocalBase != b.Manifest.String() || st.Base != "" || st.LocalBytes < int64(len(v))*8/10 {
		t.Fatalf("held %d, released %d, %+v", held, released, st)
	}
	if _, err := os.Stat(filepath.Join(b.Dir, "decode_B2.rbln")); err != nil {
		t.Fatalf("the base was touched: %v", err)
	}

	// a base that cannot be held is not read
	out = t.TempDir()
	st, err = s.Pull(ctx, nm, out, PullOptions{ReadBases: []Base{b}, Hold: func(Base) (func(), bool) { return nil, false }})
	if err != nil {
		t.Fatal(err)
	}
	sameTree(t, src, out)
	if st.LocalBase != "" || st.LocalBytes != 0 {
		t.Fatalf("read a base it could not hold: %+v", st)
	}
}

// A one-level store with FrameFlat stores its chunks in frames and gives the same files back;
// compressible data takes less room than without frames.
func TestFlatStoreCanFrameItsChunks(t *testing.T) {
	ctx := context.Background()
	data := make([]byte, 6<<20)
	for i := range data { // compressible, but not chunk by chunk identical
		data[i] = byte(i>>9) ^ byte(i%7)
	}
	src := t.TempDir()
	write(t, filepath.Join(src, "decode_B2.rbln"), data)
	sizes := map[bool]int64{}
	for _, framed := range []bool{false, true} {
		b, err := bfs.Open(t.TempDir(), backend.DurabilityNone)
		if err != nil {
			t.Fatal(err)
		}
		s, err := OpenWith(ctx, b, Create{Chunker: chunker.Params{Min: 1 << 10, Avg: 4 << 10, Max: 16 << 10},
			PackSize: 1 << 20, Frame: 256 << 10, FrameFlat: framed})
		if err != nil {
			t.Fatal(err)
		}
		if (s.Config.Frame > 0) != framed || s.Config.Sub != nil {
			t.Fatalf("framed %v: config %+v", framed, s.Config)
		}
		m, _, err := s.Push(ctx, src, PushOptions{Compress: codec.ModeAuto})
		if err != nil {
			t.Fatal(err)
		}
		out := t.TempDir()
		if _, err := s.Pull(ctx, m, out, PullOptions{}); err != nil {
			t.Fatal(err)
		}
		sameTree(t, src, out)
		err = b.List(ctx, backend.Pack, func(_ string, n int64) error { sizes[framed] += n; return nil })
		if err != nil {
			t.Fatal(err)
		}
	}
	if sizes[true] <= 0 || sizes[true] >= sizes[false] {
		t.Fatalf("packs: %d B framed, %d B unframed", sizes[true], sizes[false])
	}
}
