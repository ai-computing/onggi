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
	"github.com/ai-computing/onggi/pkg/digest"
)

func openTwoLevel(t *testing.T, root string) *Store {
	t.Helper()
	b, err := bfs.Open(root, backend.DurabilityNone)
	if err != nil {
		t.Fatal(err)
	}
	sub := DefaultSub
	s, err := OpenWith(context.Background(), b, Create{Chunker: chunker.Default, Sub: &sub, PackSize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// shuffled returns the 32 KiB blocks of data in a different order: the same bytes, rearranged in
// runs shorter than a 64 KiB chunk but long enough for 4 KiB sub-chunks, as between two ATOM
// prefill variants.
func shuffled(data []byte, seed int64) []byte {
	const blk = 32 << 10
	n := len(data) / blk
	order := random(seed, n)
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	for i := n - 1; i > 0; i-- {
		j := int(order[i]) % (i + 1)
		idx[i], idx[j] = idx[j], idx[i]
	}
	out := make([]byte, 0, len(data))
	for _, i := range idx {
		out = append(out, data[i*blk:(i+1)*blk]...)
	}
	return out
}

func TestTwoLevelFindsRearrangedBlocks(t *testing.T) {
	ctx := context.Background()
	w := random(11, 8<<20) // 8 MiB, 256 blocks of 32 KiB
	src := t.TempDir()
	write(t, filepath.Join(src, "prefill_C16.rbln"), w)
	write(t, filepath.Join(src, "prefill_C128.rbln"), shuffled(w, 12))

	one, two := t.TempDir(), t.TempDir()
	_, st1, err := openStore(t, one).Push(ctx, src, PushOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s2 := openTwoLevel(t, two)
	md, st2, err := s2.Push(ctx, src, PushOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("one level: %d new bytes; two levels: %d new bytes, %d composites", st1.NewBytes, st2.NewBytes, s2.Composites())
	if st1.NewBytes < int64(len(w))*17/10 {
		t.Fatalf("one level found %d new bytes; the test data should defeat 64 KiB chunks", st1.NewBytes)
	}
	if st2.NewBytes > int64(len(w))*15/10 {
		t.Fatalf("two levels stored %d new bytes for two arrangements of %d bytes", st2.NewBytes, len(w))
	}

	// a fresh process reads the composite tables and pulls both files back
	out := t.TempDir()
	if _, err := openTwoLevel(t, two).Pull(ctx, md, out, PullOptions{Workers: 3, MaxRange: 256 << 10}); err != nil {
		t.Fatal(err)
	}
	sameTree(t, src, out)

	// pushing again uploads nothing
	_, st3, err := openTwoLevel(t, two).Push(ctx, src, PushOptions{})
	if err != nil || st3.NewBytes != 0 || st3.Packs != 0 {
		t.Fatalf("second push: %d new bytes in %d packs, %v", st3.NewBytes, st3.Packs, err)
	}
}

// Short files and short tails are one sub-chunk equal to the chunk; they must still be stored.
// A small file equal to a piece of a large one exercises sub-chunks that are whole chunks elsewhere.
func TestTwoLevelShortChunksAndNesting(t *testing.T) {
	ctx := context.Background()
	src, storeDir, out := t.TempDir(), t.TempDir(), t.TempDir()
	big := random(16, 3<<20+5_000) // ends in a short tail
	write(t, filepath.Join(src, "big.bin"), big)
	write(t, filepath.Join(src, "config.json"), []byte(`{"tiny": true}`))
	write(t, filepath.Join(src, "small.bin"), random(17, 9_000))
	for i, n := range []int{1_000, 3_000, 6_000, 12_000} {
		write(t, filepath.Join(src, "piece", string(rune('a'+i))+".bin"), big[200_000:200_000+n])
	}
	s := openTwoLevel(t, storeDir)
	md, _, err := s.Push(ctx, src, PushOptions{Compress: codec.ModeAuto, Workers: 4})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openTwoLevel(t, storeDir).Pull(ctx, md, out, PullOptions{}); err != nil {
		t.Fatal(err)
	}
	sameTree(t, src, out)
}

func TestSubKindsLimitSubChunking(t *testing.T) {
	ctx := context.Background()
	src, storeDir, out := t.TempDir(), t.TempDir(), t.TempDir()
	w := random(18, 4<<20)
	write(t, filepath.Join(src, "model.safetensors"), w)
	write(t, filepath.Join(src, "copy.safetensors"), shuffled(w, 19))
	b, _ := bfs.Open(storeDir, backend.DurabilityNone)
	sub := DefaultSub
	s, err := OpenWith(ctx, b, Create{Chunker: chunker.Default, Sub: &sub, SubKinds: DefaultSubKinds, PackSize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	md, st, err := s.Push(ctx, src, PushOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Composites() != 0 || st.NewBytes < int64(len(w))*17/10 {
		t.Fatalf("safetensors were sub-chunked: %d composites, %d new bytes", s.Composites(), st.NewBytes)
	}
	write(t, filepath.Join(src, "prefill.rbln"), shuffled(w, 20))
	md, st, err = s.Push(ctx, src, PushOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Composites() == 0 {
		t.Fatal("an .rbln file was not sub-chunked")
	}
	if _, err := openStore(t, storeDir).Pull(ctx, md, out, PullOptions{}); err != nil {
		t.Fatal(err)
	}
	sameTree(t, src, out)
}

func TestTwoLevelWithCompression(t *testing.T) {
	ctx := context.Background()
	src, storeDir, out := t.TempDir(), t.TempDir(), t.TempDir()
	w := weights(13, 3<<20)
	write(t, filepath.Join(src, "a.rbln"), w)
	write(t, filepath.Join(src, "b.rbln"), shuffled(w, 14))
	s := openTwoLevel(t, storeDir)
	md, st, err := s.Push(ctx, src, PushOptions{Compress: codec.ModeAuto})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("new %d B stored as %d B", st.NewBytes, st.PackBytes)
	if st.PackBytes >= st.NewBytes {
		t.Fatalf("sub-chunks did not compress: %d >= %d", st.PackBytes, st.NewBytes)
	}
	if _, err := openTwoLevel(t, storeDir).Pull(ctx, md, out, PullOptions{}); err != nil {
		t.Fatal(err)
	}
	sameTree(t, src, out)
}

// A store created with one level stays one level, whatever a later Open asks for.
func TestSubSettingIsStoredWithTheStore(t *testing.T) {
	dir := t.TempDir()
	openStore(t, dir)
	if s := openTwoLevel(t, dir); s.Config.Sub != nil {
		t.Fatalf("one-level store reopened with sub-chunks %+v", *s.Config.Sub)
	}
	dir2 := t.TempDir()
	openTwoLevel(t, dir2)
	if s := openStore(t, dir2); s.Config.Sub == nil || *s.Config.Sub != DefaultSub {
		t.Fatal("two-level store lost its sub-chunk setting")
	}
	b, _ := bfs.Open(t.TempDir(), backend.DurabilityNone)
	bad := chunker.Params{Min: 16 << 10, Avg: 32 << 10, Max: 64 << 10}
	if _, err := OpenWith(context.Background(), b, Create{Chunker: chunker.Default, Sub: &bad}); err == nil {
		t.Fatal("sub-chunks larger than the chunk minimum were accepted")
	}
}

func TestCompositeTableRoundTripAndChecks(t *testing.T) {
	cs := []composite{
		{Chunk: digest.Of([]byte("a")), Subs: []ChunkRef{{digest.Of([]byte("x")), 4096}, {digest.Of([]byte("y")), 1000}}},
		{Chunk: digest.Of([]byte("b")), Subs: []ChunkRef{{digest.Of([]byte("z")), 2048}}},
	}
	data := encodeComposites(cs)
	got, err := decodeComposites(data)
	if err != nil || len(got) != 2 || got[0].Subs[1] != cs[0].Subs[1] || got[1].Chunk != cs[1].Chunk {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	for _, bad := range [][]byte{data[:len(data)-1], append(data, 0), data[:10]} {
		if _, err := decodeComposites(bad); err == nil {
			t.Errorf("accepted a damaged table of %d bytes", len(bad))
		}
	}
}

func TestPullRejectsCompositeWithMissingSubChunk(t *testing.T) {
	ctx := context.Background()
	src, storeDir := t.TempDir(), t.TempDir()
	write(t, filepath.Join(src, "a.bin"), random(15, 1<<20))
	s := openTwoLevel(t, storeDir)
	md, _, err := s.Push(ctx, src, PushOptions{})
	if err != nil {
		t.Fatal(err)
	}
	md = withoutLocator(t, s, md) // the index path: a manifest from before locators
	// drop the pack indexes: composites remain, their sub-chunks are gone
	filepath.WalkDir(filepath.Join(storeDir, "index"), func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			data, _ := os.ReadFile(p)
			if len(data) >= 8 && string(data[:8]) == string(indexMagic[:]) {
				os.Chmod(p, 0o644)
				os.Remove(p)
			}
		}
		return nil
	})
	out := t.TempDir()
	if _, err := openTwoLevel(t, storeDir).Pull(ctx, md, out, PullOptions{}); err == nil {
		t.Fatal("pull succeeded without the sub-chunks")
	}
}

// With a flush threshold of one, composites are published while the push runs, as soon as their
// sub-chunks are indexed; the result must be the same.
func TestCompositesPublishedDuringPush(t *testing.T) {
	old := compFlushAt
	compFlushAt = 1
	defer func() { compFlushAt = old }()
	ctx := context.Background()
	src, storeDir, out := twoLevelSource(t), t.TempDir(), t.TempDir()
	s := openTwoLevel(t, storeDir)
	md, _, err := s.Push(ctx, src, PushOptions{Workers: 4, Uploads: 3})
	if err != nil {
		t.Fatal(err)
	}
	tables := 0
	filepath.WalkDir(filepath.Join(storeDir, "index"), func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if data, _ := os.ReadFile(p); len(data) >= 8 && string(data[:8]) == string(compMagic[:]) {
				tables++
			}
		}
		return nil
	})
	if tables < 2 {
		t.Fatalf("%d composite tables; want several, written during the push", tables)
	}
	if _, err := openTwoLevel(t, storeDir).Pull(ctx, withoutLocator(t, s, md), out, PullOptions{}); err != nil {
		t.Fatal(err)
	}
	sameTree(t, src, out)
	_, st, err := openTwoLevel(t, storeDir).Push(ctx, src, PushOptions{})
	if err != nil || st.NewBytes != 0 {
		t.Fatalf("second push: %d new bytes, %v", st.NewBytes, err)
	}
}
