package chunker

import (
	"bytes"
	"crypto/sha256"
	"io"
	"math/rand"
	"testing"
)

func random(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func chunks(t testing.TB, data []byte, p Params) [][]byte {
	t.Helper()
	c := New(bytes.NewReader(data), p)
	var out [][]byte
	for {
		ch, err := c.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, append([]byte(nil), ch...))
	}
}

func TestReassemblyIsByteIdentical(t *testing.T) {
	for _, n := range []int{0, 1, Default.Min, Default.Max, Default.Max + 1, 3<<20 + 17, 20 << 20} {
		data := random(int64(n), n)
		var got []byte
		for _, ch := range chunks(t, data, Default) {
			got = append(got, ch...)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("n=%d: reassembled bytes differ", n)
		}
	}
}

func TestChunkSizesStayInBounds(t *testing.T) {
	cs := chunks(t, random(1, 32<<20), Default)
	total := 0
	for i, ch := range cs {
		if len(ch) > Default.Max || (len(ch) < Default.Min && i != len(cs)-1) {
			t.Fatalf("chunk %d has %d bytes", i, len(ch))
		}
		total += len(ch)
	}
	avg := total / len(cs)
	if avg < Default.Avg/2 || avg > Default.Avg*2 {
		t.Fatalf("average chunk %d bytes, want near %d", avg, Default.Avg)
	}
}

// The property the design depends on: the same content at a shifted offset yields the same
// chunks, apart from the few around the shift.
func TestShiftedContentSharesChunks(t *testing.T) {
	base := random(7, 16<<20)
	shifted := append(random(8, 12345), base...)
	seen := map[[32]byte]bool{}
	for _, ch := range chunks(t, base, Default) {
		seen[sha256.Sum256(ch)] = true
	}
	shared := 0
	for _, ch := range chunks(t, shifted, Default) {
		if seen[sha256.Sum256(ch)] {
			shared += len(ch)
		}
	}
	if f := float64(shared) / float64(len(base)); f < 0.97 {
		t.Fatalf("only %.3f of the shifted content shared", f)
	}
}

func TestDeterministicAcrossReadSizes(t *testing.T) {
	data := random(3, 9<<20)
	a := chunks(t, data, Default)
	c := New(io.LimitReader(&slowReader{data: data, step: 4093}, int64(len(data))), Default)
	i := 0
	for {
		ch, err := c.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(ch, a[i]) {
			t.Fatalf("chunk %d differs with short reads", i)
		}
		i++
	}
	if i != len(a) {
		t.Fatalf("%d chunks with short reads, %d without", i, len(a))
	}
}

type slowReader struct {
	data []byte
	step int
}

func (r *slowReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := r.step
	if n > len(p) {
		n = len(p)
	}
	if n > len(r.data) {
		n = len(r.data)
	}
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

func TestValidate(t *testing.T) {
	for _, p := range []Params{{0, 64, 128}, {128, 64, 256}, {16, 100, 256}, {16, 32, 64}} {
		if p.Validate() == nil {
			t.Errorf("%+v accepted", p)
		}
	}
	if err := Default.Validate(); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkChunker(b *testing.B) {
	data := random(9, 64<<20)
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c := New(bytes.NewReader(data), Default)
		for {
			if _, err := c.Next(); err == io.EOF {
				break
			}
		}
	}
}

// Split must cut exactly where a Chunker cuts: stores made by either must dedup against each other.
func TestSplitMatchesChunker(t *testing.T) {
	for _, p := range []Params{Default, {Min: 1 << 10, Avg: 4 << 10, Max: 16 << 10}} {
		for _, n := range []int{0, 100, p.Min, p.Max, p.Max + 1, 70_001, 3 << 20} {
			data := random(int64(n)+7, n)
			want := chunks(t, data, p)
			var got [][]byte
			if err := p.Split(data, func(c []byte) error { got = append(got, c); return nil }); err != nil {
				t.Fatal(err)
			}
			if len(got) != len(want) {
				t.Fatalf("%+v, %d bytes: Split made %d chunks, Chunker %d", p, n, len(got), len(want))
			}
			for i := range got {
				if !bytes.Equal(got[i], want[i]) {
					t.Fatalf("%+v, %d bytes: chunk %d differs", p, n, i)
				}
			}
		}
	}
}
