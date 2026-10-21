package codec

import (
	"bytes"
	"encoding/binary"
	"math"
	"math/rand"
	"testing"
)

// bf16 returns n bf16 values drawn like trained weights: normal, small magnitude.
func bf16(seed int64, n int) []byte {
	r := rand.New(rand.NewSource(seed))
	b := make([]byte, 2*n)
	for i := 0; i < n; i++ {
		f := float32(r.NormFloat64() * 0.02)
		binary.LittleEndian.PutUint16(b[2*i:], uint16(math.Float32bits(f)>>16))
	}
	return b
}

func TestRoundTrip(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	random := make([]byte, 70001)
	r.Read(random)
	cases := map[string][]byte{
		"empty":    {},
		"tiny":     []byte("abc"),
		"odd text": bytes.Repeat([]byte("onggi chunk "), 5001)[:60001],
		"random":   random,
		"bf16":     bf16(2, 32768),
		"zeros":    make([]byte, 65536),
	}
	for name, raw := range cases {
		for _, m := range []Mode{ModeOff, ModeZstd, ModeAuto} {
			stored, id := Encode(raw, m)
			got, err := Decode(stored, id, len(raw))
			if err != nil || !bytes.Equal(got, raw) {
				t.Fatalf("%s/%s: codec %s round trip failed: %v", name, m, id, err)
			}
		}
	}
}

func TestChoices(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	random := make([]byte, 65536)
	r.Read(random)
	if _, id := Encode(random, ModeAuto); id != None {
		t.Errorf("random bytes stored as %s, want none", id)
	}
	w := bf16(4, 32768)
	stored, id := Encode(w, ModeAuto)
	if id != Shuffle2Zstd {
		t.Errorf("bf16 stored as %s, want shuffle2+zstd", id)
	}
	z, _ := Encode(w, ModeZstd)
	t.Logf("bf16 64 KiB: zstd %d B, auto %d B (%.2fx)", len(z), len(stored), float64(len(w))/float64(len(stored)))
	if len(stored) >= len(z) {
		t.Errorf("shuffle did not help on bf16: %d >= %d", len(stored), len(z))
	}
	if _, id := Encode(w, ModeOff); id != None {
		t.Errorf("mode none gave %s", id)
	}
}

func TestDecodeRejectsBadInput(t *testing.T) {
	if _, err := Decode([]byte("xyz"), Zstd, 3); err == nil {
		t.Error("garbage zstd accepted")
	}
	stored, id := Encode(bytes.Repeat([]byte("a"), 1000), ModeZstd)
	if _, err := Decode(stored, id, 999); err == nil {
		t.Error("wrong length accepted")
	}
	if _, err := Decode([]byte("ab"), ID(9), 2); err == nil {
		t.Error("unknown codec accepted")
	}
	if _, err := ParseMode("lzma"); err == nil {
		t.Error("ParseMode accepted lzma")
	}
}

func BenchmarkAutoBF16(b *testing.B) {
	w := bf16(5, 32768)
	b.SetBytes(int64(len(w)))
	for i := 0; i < b.N; i++ {
		Encode(w, ModeAuto)
	}
}

func BenchmarkDecodeBF16(b *testing.B) {
	w := bf16(6, 32768)
	stored, id := Encode(w, ModeAuto)
	b.SetBytes(int64(len(w)))
	for i := 0; i < b.N; i++ {
		Decode(stored, id, len(w))
	}
}
