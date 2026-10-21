package store

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/ai-computing/onggi/pkg/codec"
	"github.com/ai-computing/onggi/pkg/digest"
)

// weights returns n bf16 values shaped like trained weights.
func weights(seed int64, n int) []byte {
	r := rand.New(rand.NewSource(seed))
	b := make([]byte, 2*n)
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(math.Float32bits(float32(r.NormFloat64()*0.02))>>16))
	}
	return b
}

func codecsIn(s *Store) map[codec.ID]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[codec.ID]int{}
	for _, l := range s.index {
		out[l.Codec]++
	}
	return out
}

func TestCompressedRoundTrip(t *testing.T) {
	ctx := context.Background()
	src, storeDir, out := t.TempDir(), t.TempDir(), t.TempDir()
	w := weights(1, 2<<20) // 4 MiB of bf16
	write(t, filepath.Join(src, "model.safetensors"), w)
	write(t, filepath.Join(src, "graph.edf"), bytes.Repeat([]byte("node matmul tile 128x128 "), 40_000))
	write(t, filepath.Join(src, "noise.bin"), random(9, 1<<20))
	write(t, filepath.Join(src, "odd.bin"), weights(2, 33_333)[:66_665])

	s := openStore(t, storeDir)
	md, st, err := s.Push(ctx, src, PushOptions{Compress: codec.ModeAuto})
	if err != nil {
		t.Fatal(err)
	}
	if st.PackBytes >= st.NewBytes {
		t.Fatalf("auto stored %d bytes for %d new bytes", st.PackBytes, st.NewBytes)
	}
	used := codecsIn(s)
	if used[codec.Shuffle2Huffman] == 0 || used[codec.Zstd] == 0 || used[codec.None] == 0 {
		t.Fatalf("codecs used %v, want all three (weights, graph text, noise)", used)
	}
	t.Logf("new %d B stored as %d B (%.2fx); codecs %v", st.NewBytes, st.PackBytes, float64(st.NewBytes)/float64(st.PackBytes), used)

	if _, err := openStore(t, storeDir).Pull(ctx, md, out, PullOptions{Workers: 3, MaxRange: 256 << 10}); err != nil {
		t.Fatal(err)
	}
	sameTree(t, src, out)
}

// A store written without compression (or by an older onggi) and one push with it mix freely.
func TestMixedCodecsInOneStore(t *testing.T) {
	ctx := context.Background()
	a, b, storeDir := t.TempDir(), t.TempDir(), t.TempDir()
	w := weights(3, 1<<20)
	write(t, filepath.Join(a, "w.bin"), w)
	write(t, filepath.Join(b, "w.bin"), w)
	write(t, filepath.Join(b, "more.bin"), weights(4, 1<<20))

	s := openStore(t, storeDir)
	if _, _, err := s.Push(ctx, a, PushOptions{Compress: codec.ModeOff}); err != nil {
		t.Fatal(err)
	}
	mb, st, err := s.Push(ctx, b, PushOptions{Compress: codec.ModeAuto})
	if err != nil {
		t.Fatal(err)
	}
	if st.NewBytes != int64(len(w)) {
		t.Fatalf("second push: %d new bytes, want %d (w.bin deduplicates against the raw copy)", st.NewBytes, len(w))
	}
	out := t.TempDir()
	if _, err := openStore(t, storeDir).Pull(ctx, mb, out, PullOptions{}); err != nil {
		t.Fatal(err)
	}
	sameTree(t, b, out)
}

func TestIndexV1StillReads(t *testing.T) {
	pack := digest.Of([]byte("pack"))
	chunk := digest.Of([]byte("chunk"))
	var v1 bytes.Buffer
	v1.Write(indexMagicV1[:])
	v1.Write(pack[:])
	binary.Write(&v1, binary.LittleEndian, uint32(1))
	v1.Write(chunk[:])
	binary.Write(&v1, binary.LittleEndian, uint32(4096))
	binary.Write(&v1, binary.LittleEndian, uint32(65536))
	_, entries, err := decodeIndex(v1.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	want := Loc{Pack: pack, Offset: 4096, Stored: 65536, Length: 65536, Codec: codec.None}
	if len(entries) != 1 || entries[0].Chunk != chunk || entries[0].Loc != want {
		t.Fatalf("v1 index decoded as %+v", entries)
	}

	e := []indexEntry{{Chunk: chunk, Loc: Loc{Pack: pack, Offset: 7, Stored: 100, Length: 300, Codec: codec.Shuffle2Huffman}}}
	_, got, err := decodeIndex(encodeIndex(pack, e))
	if err != nil || len(got) != 1 || got[0] != e[0] {
		t.Fatalf("v2 round trip: %+v %v", got, err)
	}
}

func TestPullDetectsCorruptCompressedChunk(t *testing.T) {
	ctx := context.Background()
	src, storeDir := t.TempDir(), t.TempDir()
	write(t, filepath.Join(src, "w.bin"), weights(5, 1<<20))
	s := openStore(t, storeDir)
	md, _, err := s.Push(ctx, src, PushOptions{Compress: codec.ModeAuto})
	if err != nil {
		t.Fatal(err)
	}
	filepath.WalkDir(filepath.Join(storeDir, "packs"), func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			data, _ := os.ReadFile(p)
			data[len(data)/3] ^= 0x5a
			os.Chmod(p, 0o644)
			os.WriteFile(p, data, 0o644)
		}
		return nil
	})
	out := t.TempDir()
	if _, err := openStore(t, storeDir).Pull(ctx, md, out, PullOptions{}); err == nil {
		t.Fatal("pull of a corrupt compressed pack succeeded")
	}
	if _, serr := os.Stat(filepath.Join(out, "w.bin")); serr == nil {
		t.Fatal("a corrupt file was published under its final name")
	}
}
