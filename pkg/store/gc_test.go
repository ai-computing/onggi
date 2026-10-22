package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ai-computing/onggi/pkg/backend"
	bfs "github.com/ai-computing/onggi/pkg/backend/fs"
	"github.com/ai-computing/onggi/pkg/chunker"
	"github.com/ai-computing/onggi/pkg/codec"
	"github.com/ai-computing/onggi/pkg/digest"
)

func gcStore(t *testing.T, dir string) (*Store, *bfs.Store) {
	t.Helper()
	b, err := bfs.Open(dir, backend.DurabilityNone)
	if err != nil {
		t.Fatal(err)
	}
	sub := DefaultSub
	s, err := OpenWith(context.Background(), b, Create{Chunker: chunker.Default, Sub: &sub, PackSize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	return s, b
}

func pushRef(t *testing.T, s *Store, refs backend.Refs, dir, ref string) digest.Digest {
	t.Helper()
	ctx := context.Background()
	md, _, err := s.Push(ctx, dir, PushOptions{Compress: codec.ModeAuto})
	if err != nil {
		t.Fatal(err)
	}
	old, _ := refs.GetRef(ctx, ref)
	if err := refs.SetRef(ctx, ref, old, md.String()); err != nil {
		t.Fatal(err)
	}
	return md
}

func dirBytes(t *testing.T, root string, sub string) int64 {
	var n int64
	filepath.WalkDir(filepath.Join(root, sub), func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			info, _ := d.Info()
			n += info.Size()
		}
		return nil
	})
	return n
}

func clock(t0 time.Time, d time.Duration) func() time.Time {
	return func() time.Time { return t0.Add(d) }
}

func TestGCDeletesOnlyAfterGraceAndKeepsLiveArtifacts(t *testing.T) {
	ctx := context.Background()
	storeDir := t.TempDir()
	w := weights(41, 2<<20)
	a, b := t.TempDir(), t.TempDir()
	write(t, filepath.Join(a, "decode.rbln"), w)
	write(t, filepath.Join(b, "decode.rbln"), w)                                              // shared with a
	write(t, filepath.Join(b, "prefill.rbln"), append(shuffled(w, 42), random(43, 1<<20)...)) // partly new
	write(t, filepath.Join(b, "only-b.bin"), random(44, 3<<20))                               // only b

	s, refs := gcStore(t, storeDir)
	ma := pushRef(t, s, refs, a, "a")
	pushRef(t, s, refs, b, "b")
	before := dirBytes(t, storeDir, "packs")
	if err := refs.SetRef(ctx, "b", mustRef(t, refs, "b"), ""); err != nil {
		t.Fatal(err)
	}

	t0 := time.Now()
	grace := 24 * time.Hour
	run := func(after time.Duration) GCStats {
		t.Helper()
		g, _ := gcStore(t, storeDir) // a fresh process each run
		st, err := g.GC(ctx, refs, GCOptions{Grace: grace, LeaseTTL: 1000 * time.Hour, Now: clock(t0, after)})
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	st1 := run(0)
	if st1.Phase != "marked" || st1.Candidates == 0 || st1.Deleted != 0 {
		t.Fatalf("first run: %+v", st1)
	}
	if st := run(time.Hour); st.Phase != "waiting" || st.Deleted != 0 {
		t.Fatalf("run inside the grace period: %+v", st)
	}
	st3 := run(25 * time.Hour)
	if st3.Phase != "swept" || st3.Deleted == 0 {
		t.Fatalf("run after the grace period: %+v", st3)
	}
	after := dirBytes(t, storeDir, "packs")
	t.Logf("packs %d B -> %d B; deleted %d objects, %d B; dead bytes in live packs %d", before, after, st3.Deleted, st3.DeletedBytes, st3.DeadInLive)
	if after >= before {
		t.Fatalf("packs did not shrink: %d -> %d", before, after)
	}

	// a still pulls byte-identical from a fresh process; nothing of a was swept
	out := t.TempDir()
	g, _ := gcStore(t, storeDir)
	if _, err := g.Pull(ctx, ma, out, PullOptions{}); err != nil {
		t.Fatal(err)
	}
	sameTree(t, a, out)
	// a store index loaded after the sweep has no composite naming a missing sub-chunk
	assertIndexConsistent(t, g)
	// b can be pushed again
	pushRef(t, g, refs, b, "b")
	out = t.TempDir()
	mb, _ := refs.GetRef(ctx, "b")
	d, _ := digest.Parse(mb)
	g2, _ := gcStore(t, storeDir)
	if _, err := g2.Pull(ctx, d, out, PullOptions{}); err != nil {
		t.Fatal(err)
	}
	sameTree(t, b, out)
}

func mustRef(t *testing.T, refs backend.Refs, name string) string {
	t.Helper()
	v, err := refs.GetRef(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func assertIndexConsistent(t *testing.T, s *Store) {
	t.Helper()
	if err := s.LoadIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i, e := range s.idx.ent {
		if e.state != stateComp {
			continue
		}
		for _, sid := range s.idx.subsOf(e) {
			if s.idx.ent[sid].state == stateUnknown {
				t.Fatalf("composite %s names sub-chunk %s, which the store no longer holds", s.idx.dig[i], s.idx.dig[sid])
			}
		}
	}
}

// One push writes the composites of both files into one table. When only one file stays live, the
// table is rewritten with the live composites, and the old one goes after its own grace period.
func TestGCRewritesMixedCompositeTables(t *testing.T) {
	ctx := context.Background()
	storeDir := t.TempDir()
	w := weights(45, 2<<20)
	x, y := t.TempDir(), t.TempDir()
	write(t, filepath.Join(x, "keep.rbln"), w)
	write(t, filepath.Join(x, "drop.rbln"), append(random(46, 1<<20), shuffled(w, 47)...))
	write(t, filepath.Join(y, "keep.rbln"), w)

	s, refs := gcStore(t, storeDir)
	pushRef(t, s, refs, x, "x")
	my := pushRef(t, s, refs, y, "y") // dedups fully against x
	if err := refs.SetRef(ctx, "x", mustRef(t, refs, "x"), ""); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	run := func(after time.Duration) GCStats {
		g, _ := gcStore(t, storeDir)
		st, err := g.GC(ctx, refs, GCOptions{Grace: time.Hour, LeaseTTL: 1000 * time.Hour, Now: clock(t0, after)})
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	st1 := run(0)
	if st1.Rewritten == 0 {
		t.Fatalf("no table rewritten: %+v", st1)
	}
	run(2 * time.Hour) // deletes the first candidates, records the superseded table
	run(4 * time.Hour) // deletes the superseded table
	g, _ := gcStore(t, storeDir)
	assertIndexConsistent(t, g)
	if st := run(6 * time.Hour); st.Candidates != 0 {
		t.Fatalf("garbage left after three sweeps: %+v", st)
	}
	out := t.TempDir()
	if _, err := g.Pull(ctx, my, out, PullOptions{}); err != nil {
		t.Fatal(err)
	}
	sameTree(t, y, out)
}

func TestGCLeaseBlocksTheSweep(t *testing.T) {
	ctx := context.Background()
	storeDir := t.TempDir()
	a := t.TempDir()
	write(t, filepath.Join(a, "w.bin"), random(48, 2<<20))
	s, refs := gcStore(t, storeDir)
	pushRef(t, s, refs, a, "a")
	if err := refs.SetRef(ctx, "a", mustRef(t, refs, "a"), ""); err != nil {
		t.Fatal(err)
	}
	release, err := s.Lease(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	o := func(after time.Duration) GCOptions {
		return GCOptions{Grace: time.Hour, LeaseTTL: 1000 * time.Hour, Now: clock(t0, after)}
	}
	if st, err := s.GC(ctx, refs, o(0)); err != nil || st.Phase != "marked" {
		t.Fatalf("%+v %v", st, err)
	}
	if st, err := s.GC(ctx, refs, o(2*time.Hour)); err != nil || st.Phase != "blocked" || st.Deleted != 0 {
		t.Fatalf("with a live lease: %+v %v", st, err)
	}
	release()
	if st, err := s.GC(ctx, refs, o(3*time.Hour)); err != nil || st.Phase != "swept" || st.Deleted == 0 {
		t.Fatalf("after release: %+v %v", st, err)
	}
	// an expired lease (crashed push) does not block
	release2, _ := s.Lease(ctx, "crashed")
	defer release2()
	s.GC(ctx, refs, GCOptions{Grace: time.Hour, LeaseTTL: time.Minute, Now: clock(t0, 4*time.Hour)})
	if st, err := s.GC(ctx, refs, GCOptions{Grace: time.Hour, LeaseTTL: time.Minute, Now: clock(t0, 6*time.Hour)}); err != nil || st.Phase == "blocked" {
		t.Fatalf("expired lease blocked GC: %+v %v", st, err)
	}
}

func TestGCDeletesNothingWhenARootIsUnreadable(t *testing.T) {
	ctx := context.Background()
	storeDir := t.TempDir()
	a := t.TempDir()
	write(t, filepath.Join(a, "w.bin"), random(49, 1<<20))
	s, refs := gcStore(t, storeDir)
	pushRef(t, s, refs, a, "a")
	if err := refs.SetRef(ctx, "broken", "", "sha256:"+digest.Of([]byte("nothing")).Hex()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GC(ctx, refs, GCOptions{}); err == nil {
		t.Fatal("GC ran with an unreadable root")
	}
	if _, err := readAll(ctx, s.B, gcStateKey); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("GC state written despite the error: %v", err)
	}
}

func TestGCDryRunChangesNothing(t *testing.T) {
	ctx := context.Background()
	storeDir := t.TempDir()
	a := t.TempDir()
	write(t, filepath.Join(a, "w.bin"), random(50, 1<<20))
	s, refs := gcStore(t, storeDir)
	pushRef(t, s, refs, a, "a")
	refs.SetRef(ctx, "a", mustRef(t, refs, "a"), "")
	st, err := s.GC(ctx, refs, GCOptions{DryRun: true})
	if err != nil || st.Phase != "dry-run" || st.Candidates == 0 {
		t.Fatalf("%+v %v", st, err)
	}
	if _, err := readAll(ctx, s.B, gcStateKey); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("dry run wrote state: %v", err)
	}
}

// Live and dead chunks in the same packs: without repack the dead bytes stay; with it the live
// chunks move to new packs, the old packs go after the grace period, and pulls whose locator names
// a removed pack still work through the index.
func TestGCRepacksMostlyDeadPacks(t *testing.T) {
	ctx := context.Background()
	storeDir := t.TempDir()
	mixed := t.TempDir()
	// one push interleaves a kept file and a dropped file in the same packs
	write(t, filepath.Join(mixed, "a-keep.bin"), random(51, 3<<20))
	write(t, filepath.Join(mixed, "b-drop.bin"), random(52, 3<<20))
	keepOnly := t.TempDir()
	write(t, filepath.Join(keepOnly, "a-keep.bin"), random(51, 3<<20))

	s, refs := gcStore(t, storeDir)
	pushRef(t, s, refs, mixed, "mixed")
	mk := pushRef(t, s, refs, keepOnly, "keep") // dedups fully: its locator names the mixed packs
	refs.SetRef(ctx, "mixed", mustRef(t, refs, "mixed"), "")
	before := dirBytes(t, storeDir, "packs")

	t0 := time.Now()
	run := func(after time.Duration) GCStats {
		g, _ := gcStore(t, storeDir)
		st, err := g.GC(ctx, refs, GCOptions{Grace: time.Hour, LeaseTTL: 1000 * time.Hour, RepackDead: 0.25, Now: clock(t0, after)})
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	st1 := run(0)
	t.Logf("run 1: %+v", st1)
	run(2 * time.Hour)
	st3 := run(4 * time.Hour)
	t.Logf("run 3: %+v", st3)
	after := dirBytes(t, storeDir, "packs")
	t.Logf("packs %d B -> %d B", before, after)
	if st1.Repacked == 0 || after > before*6/10 {
		t.Fatalf("repack reclaimed too little: %d -> %d (repacked %d)", before, after, st1.Repacked)
	}
	// a live pack with less than RepackDead dead bytes stays; which dead chunks share a pack with
	// live ones depends on how the parallel push packed them (#44)
	if st := run(6 * time.Hour); st.Candidates != 0 || st.DeadInLive > after/4 {
		t.Fatalf("left after repack and sweeps: %+v", st)
	}
	out := t.TempDir()
	g, _ := gcStore(t, storeDir)
	if _, err := g.Pull(ctx, mk, out, PullOptions{}); err != nil {
		t.Fatal(err)
	}
	sameTree(t, keepOnly, out)
	assertIndexConsistent(t, g)
}
