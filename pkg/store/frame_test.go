package store

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/ai-computing/onggi/pkg/backend"
	bfs "github.com/ai-computing/onggi/pkg/backend/fs"
	"github.com/ai-computing/onggi/pkg/chunker"
	"github.com/ai-computing/onggi/pkg/codec"
	"github.com/ai-computing/onggi/pkg/digest"
)

func openFramed(t *testing.T, root string, frame int64) *Store {
	t.Helper()
	b, err := bfs.Open(root, backend.DurabilityNone)
	if err != nil {
		t.Fatal(err)
	}
	sub := DefaultSub
	s, err := OpenWith(context.Background(), b, Create{Chunker: chunker.Default, Sub: &sub, PackSize: 1 << 20, Frame: frame})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// compressible returns data that does not dedup but compresses across chunks: a random 16 KiB
// block repeated with a counter written every 512 bytes, so every sub-chunk differs while a frame
// finds the repeats.
func compressible(seed, n int) []byte {
	block := random(int64(seed), 16<<10)
	var b bytes.Buffer
	for i := 0; b.Len() < n; i++ {
		blk := append([]byte(nil), block...)
		for o := 0; o < len(blk); o += 512 {
			copy(blk[o:], fmt.Sprintf("%08x", i*64+o))
		}
		b.Write(blk)
	}
	return b.Bytes()[:n]
}

func framedEntries(t *testing.T, s *Store) (framed, all int) {
	t.Helper()
	if err := s.LoadIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, e := range s.idx.ent {
		if e.state == stateStored {
			all++
			if e.loc.Frame != 0 {
				framed++
			}
		}
	}
	return framed, all
}

// Frames: smaller packs than chunk-by-chunk compression, identical pulls, dedup kept (#52).
func TestFramesRoundTripAndCompressBetter(t *testing.T) {
	ctx := context.Background()
	base := append(random(81, 2<<20), compressible(1, 2<<20)...)
	next := append([]byte(nil), base...)
	copy(next[3<<20:], compressible(2, 256<<10)) // a compressible region changes
	src, src2 := t.TempDir(), t.TempDir()
	write(t, filepath.Join(src, "decode_B2.rbln"), base)
	write(t, filepath.Join(src2, "decode_B8.rbln"), next)

	var packBytes [2]int64
	for k, frame := range []int64{0, DefaultFrame} {
		root := t.TempDir()
		s := openFramed(t, root, frame)
		m1, st1, err := s.Push(ctx, src, PushOptions{Compress: codec.ModeAuto})
		if err != nil {
			t.Fatal(err)
		}
		m2, st2, err := s.Push(ctx, src2, PushOptions{Compress: codec.ModeAuto})
		if err != nil {
			t.Fatal(err)
		}
		packBytes[k] = st2.PackBytes
		t.Logf("frame %d: first push %d B packed, second %d B packed", frame, st1.PackBytes, st2.PackBytes)
		for _, c := range []struct {
			m   digest.Digest
			src string
		}{{m1, src}, {m2, src2}} {
			out := t.TempDir()
			g := openFramed(t, root, 0) // a fresh reader of the same store
			if _, err := g.Pull(ctx, c.m, out, PullOptions{}); err != nil {
				t.Fatal(err)
			}
			sameTree(t, c.src, out)
		}
		framed, all := framedEntries(t, openFramed(t, root, 0))
		if (frame == 0) != (framed == 0) || all == 0 {
			t.Fatalf("frame %d: %d of %d entries framed", frame, framed, all)
		}
		if st2.NewBytes > 1<<20 {
			t.Fatalf("frame %d: the variant stored %d new bytes; dedup lost", frame, st2.NewBytes)
		}
	}
	if packBytes[1]*2 > packBytes[0] {
		t.Fatalf("frames did not help the variant: %d B framed vs %d B chunk by chunk", packBytes[1], packBytes[0])
	}
}

// A framed store feeds a node cache, peers and repack, each of which takes chunks one by one.
func TestFramesThroughCachePeersAndRepack(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s := openFramed(t, root, DefaultFrame)
	src := t.TempDir()
	write(t, filepath.Join(src, "decode_B2.rbln"), append(compressible(3, 1<<20), random(82, 1<<20)...))
	m, _, err := s.Push(ctx, src, PushOptions{Compress: codec.ModeAuto})
	if err != nil {
		t.Fatal(err)
	}
	// node cache: filled from frames, then read alone
	cacheDir := t.TempDir()
	if _, err := s.Pull(ctx, m, t.TempDir(), PullOptions{Cache: openCache(t, cacheDir, s)}); err != nil {
		t.Fatal(err)
	}
	c := openCache(t, cacheDir, s)
	if framed, all := framedEntries(t, c); framed != 0 || all == 0 {
		t.Fatalf("cache: %d of %d entries framed", framed, all)
	}
	out := t.TempDir()
	st, err := s.Pull(ctx, m, out, PullOptions{Cache: openCache(t, cacheDir, s)})
	if err != nil {
		t.Fatal(err)
	}
	sameTree(t, src, out)
	if st.ReadBytes != 0 {
		t.Fatalf("second pull read %d B from the store", st.ReadBytes)
	}
	// the framed store as a peer
	out = t.TempDir()
	st, err = s.Pull(ctx, m, out, PullOptions{Peers: []ChunkSource{LocalSource("peer", s)}})
	if err != nil {
		t.Fatal(err)
	}
	sameTree(t, src, out)
	if st.PeerChunks == 0 {
		t.Fatalf("no chunks from the framed peer: %+v", st)
	}
	// repack after half the frames' chunks die
	refs := s.B.(*bfs.Store)
	if err := refs.SetRef(ctx, "keep", "", m.String()); err != nil {
		t.Fatal(err)
	}
	src2 := t.TempDir()
	write(t, filepath.Join(src2, "other.rbln"), compressible(4, 1<<20))
	m2, _, err := s.Push(ctx, src2, PushOptions{Compress: codec.ModeAuto})
	if err != nil {
		t.Fatal(err)
	}
	if err := refs.SetRef(ctx, "drop", "", m2.String()); err != nil {
		t.Fatal(err)
	}
	if err := refs.SetRef(ctx, "drop", m2.String(), ""); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	for _, after := range []time.Duration{0, 2 * time.Hour, 4 * time.Hour} {
		g := openFramed(t, root, 0)
		if _, err := g.GC(ctx, refs, GCOptions{Grace: time.Hour, RepackDead: 0.25, Now: clock(t0, after)}); err != nil {
			t.Fatal(err)
		}
	}
	out = t.TempDir()
	if _, err := openFramed(t, root, 0).Pull(ctx, m, out, PullOptions{}); err != nil {
		t.Fatal(err)
	}
	sameTree(t, src, out)
	assertIndexConsistent(t, openFramed(t, root, 0))
}

func TestIndexV3RoundTrip(t *testing.T) {
	var pack digest.Digest
	pack[0] = 7
	in := []indexEntry{
		{Chunk: digest.Digest{1}, Loc: Loc{Offset: 0, Stored: 100, Length: 4096, Codec: codec.Zstd, Inner: 0, Frame: 8192}},
		{Chunk: digest.Digest{2}, Loc: Loc{Offset: 0, Stored: 100, Length: 4096, Codec: codec.Zstd, Inner: 4096, Frame: 8192}},
		{Chunk: digest.Digest{3}, Loc: Loc{Offset: 100, Stored: 50, Length: 60, Codec: codec.None}},
	}
	data := encodeIndex(pack, in)
	if !bytes.HasPrefix(data, indexMagicV3[:]) {
		t.Fatal("framed entries not written as version 3")
	}
	p, out, err := decodeIndex(data)
	if err != nil || p != pack || len(out) != 3 {
		t.Fatalf("%v %v %d", err, p, len(out))
	}
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("entry %d: %+v, want %+v", i, out[i], in[i])
		}
	}
	if !bytes.HasPrefix(encodeIndex(pack, in[2:]), indexMagic[:]) {
		t.Fatal("an index without frames is not version 2")
	}
	bad := append([]indexEntry(nil), in...)
	bad[1].Loc.Inner = 8000 // runs past the frame
	if _, _, err := decodeIndex(encodeIndex(pack, bad)); err == nil {
		t.Fatal("accepted a chunk outside its frame")
	}
}
