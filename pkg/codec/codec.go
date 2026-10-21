// Package codec compresses single chunks after dedup.
//
// Chunk digests are always taken over the raw bytes, so a chunk deduplicates the same way however
// it is stored. Codecs:
//
//   - None: raw bytes.
//   - Zstd: Zstandard (RFC 8878). Suits graphs, configs and text, which have repeats.
//   - Shuffle2Huffman: the bytes of each 2-byte word split into two planes (low bytes, high
//     bytes), each plane Huffman-coded on its own. This is the byte-shuffle filter of HDF5 and
//     Blosc followed by entropy coding, as in ZipNN and DFloat11 (docs/related-work.md §2). bf16
//     and fp16 weights have few repeats, so an LZ stage finds nothing (Zstandard's fast levels then
//     store such data raw); their high plane (sign and exponent) has 2–3 bits of entropy and the
//     low plane a little under 8.
//
// Shuffle2Huffman layout: uint32 length of the first plane record, then two plane records (low,
// high). A plane record is one mode byte and its data: 0 raw bytes, 1 a Huffman table and 4-stream
// Huffman data, 2 a single byte repeated over the plane.
//
// Auto tries Zstd and Shuffle2Huffman and keeps the smaller; a chunk that does not shrink by at
// least 3 % is stored raw.
package codec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"sync"

	"github.com/klauspost/compress/huff0"
	"github.com/klauspost/compress/zstd"
)

// ID names a codec in the pack index. Values are stored; never renumber.
type ID uint8

const (
	None            ID = 0
	Zstd            ID = 1
	Shuffle2Huffman ID = 2
)

func (id ID) String() string {
	switch id {
	case None:
		return "none"
	case Zstd:
		return "zstd"
	case Shuffle2Huffman:
		return "shuffle2+huffman"
	}
	return fmt.Sprintf("codec(%d)", uint8(id))
}

// Mode selects how Encode picks a codec.
type Mode string

const (
	ModeOff  Mode = "none" // store raw
	ModeZstd Mode = "zstd" // Zstd only
	ModeAuto Mode = "auto" // Zstd or Shuffle2Huffman, whichever is smaller
)

// ParseMode accepts none, zstd or auto.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(s); m {
	case ModeOff, ModeZstd, ModeAuto:
		return m, nil
	}
	return "", fmt.Errorf("compression %q: want none, zstd or auto", s)
}

// minGain: keep a compressed form only if it is at most 97 % of the raw size.
const minGainNum, minGainDen = 97, 100

// maxPlane is the largest plane huff0 codes in one block; larger chunks skip Shuffle2Huffman.
const maxPlane = huff0.BlockSizeMax

// maxChunk bounds the chunks the store hands to Encode (the chunker's largest maximum).
const maxChunk = 256 << 10

// scratches are reused across decodes: a fresh huff0.Scratch allocates its output on every call.
var scratches = sync.Pool{New: func() any { return new(huff0.Scratch) }}

const (
	planeRaw  = 0
	planeHuff = 1
	planeRLE  = 2
)

var (
	coderOnce sync.Once
	enc       *zstd.Encoder
	dec       *zstd.Decoder
)

func coders() (*zstd.Encoder, *zstd.Decoder) {
	coderOnce.Do(func() {
		// EncodeAll/DecodeAll are safe for concurrent use; the concurrency options bound how many
		// run at once, so they follow GOMAXPROCS. A chunk is at most 256 KiB, so a 256 KiB window
		// loses nothing; the default window keeps 8 MiB of history per encoder (1.5 GB on 96
		// cores, #17).
		n := runtime.GOMAXPROCS(0)
		enc, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(n),
			zstd.WithWindowSize(maxChunk), zstd.WithLowerEncoderMem(true), zstd.WithZeroFrames(true))
		dec, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(n), zstd.WithDecoderLowmem(true),
			zstd.WithDecoderMaxMemory(1<<30))
	})
	return enc, dec
}

// Encode returns the stored form of raw and its codec. The result may alias raw (codec None).
func Encode(raw []byte, m Mode) ([]byte, ID) {
	if m == ModeOff || m == "" || len(raw) < 64 {
		return raw, None
	}
	e, _ := coders()
	best, id := e.EncodeAll(raw, make([]byte, 0, len(raw)/2)), Zstd
	if m == ModeAuto && (len(raw)+1)/2 <= maxPlane {
		if sh := encodePlanes(raw); len(sh) < len(best) {
			best, id = sh, Shuffle2Huffman
		}
	}
	if len(best)*minGainDen > len(raw)*minGainNum {
		return raw, None
	}
	return best, id
}

// Decode restores the raw bytes of a chunk of rawLen bytes.
func Decode(stored []byte, id ID, rawLen int) ([]byte, error) {
	switch id {
	case None:
		if len(stored) != rawLen {
			return nil, fmt.Errorf("raw chunk is %d bytes, want %d", len(stored), rawLen)
		}
		return stored, nil
	case Zstd:
		_, d := coders()
		out, err := d.DecodeAll(stored, make([]byte, 0, rawLen))
		if err != nil {
			return nil, err
		}
		if len(out) != rawLen {
			return nil, fmt.Errorf("%s chunk decodes to %d bytes, want %d", id, len(out), rawLen)
		}
		return out, nil
	case Shuffle2Huffman:
		return decodePlanes(stored, rawLen)
	}
	return nil, errors.New("unknown codec " + id.String())
}

// encodePlanes stores the low plane (even-index bytes) and the high plane (odd-index bytes plus an
// odd trailing byte) as two plane records.
func encodePlanes(raw []byte) []byte {
	n := len(raw) / 2
	lo := make([]byte, n)
	hi := make([]byte, len(raw)-n)
	for i := 0; i < n; i++ {
		lo[i] = raw[2*i]
		hi[i] = raw[2*i+1]
	}
	if len(raw)%2 == 1 {
		hi[n] = raw[len(raw)-1]
	}
	out := make([]byte, 4, 8+len(raw))
	out = appendPlane(out, lo)
	binary.LittleEndian.PutUint32(out, uint32(len(out)-4))
	return appendPlane(out, hi)
}

func appendPlane(dst, p []byte) []byte {
	if len(p) == 0 {
		return append(dst, planeRaw)
	}
	s := huff0.Scratch{Reuse: huff0.ReusePolicyNone}
	c, _, err := huff0.Compress4X(p, &s)
	switch {
	case err == nil && len(c) < len(p):
		return append(append(dst, planeHuff), c...)
	case errors.Is(err, huff0.ErrUseRLE):
		return append(dst, planeRLE, p[0])
	}
	return append(append(dst, planeRaw), p...)
}

// readPlane writes plane rec (n bytes) to every second byte of out, starting at out[first]. A
// plane of n bytes fills out[first], out[first+2], ..., and, for the high plane of an odd-length
// chunk, the last byte (tail).
func readPlane(rec []byte, n int, out []byte, first int, tail bool) error {
	if len(rec) == 0 {
		return errors.New("empty plane record")
	}
	data := rec[1:]
	var p []byte
	switch rec[0] {
	case planeRaw:
		if len(data) != n {
			return fmt.Errorf("raw plane is %d bytes, want %d", len(data), n)
		}
		p = data
	case planeRLE:
		if len(data) != 1 {
			return errors.New("bad RLE plane")
		}
		for i := 0; i < n; i++ {
			out[place(i, n, first, tail, len(out))] = data[0]
		}
		return nil
	case planeHuff:
		if n > maxPlane {
			return fmt.Errorf("Huffman plane of %d bytes exceeds %d", n, maxPlane)
		}
		sc := scratches.Get().(*huff0.Scratch)
		defer scratches.Put(sc)
		s, rem, err := huff0.ReadTable(data, sc)
		if err != nil {
			return err
		}
		if p, err = s.Decompress4X(rem, n); err != nil {
			return err
		}
		if len(p) != n {
			return fmt.Errorf("Huffman plane decodes to %d bytes, want %d", len(p), n)
		}
	default:
		return fmt.Errorf("unknown plane mode %d", rec[0])
	}
	for i, b := range p {
		out[place(i, n, first, tail, len(out))] = b
	}
	return nil
}

// place is where byte i of a plane goes in the chunk.
func place(i, n, first int, tail bool, size int) int {
	if tail && i == n-1 {
		return size - 1
	}
	return 2*i + first
}

func decodePlanes(stored []byte, rawLen int) ([]byte, error) {
	if len(stored) < 4 {
		return nil, errors.New("shuffle2+huffman chunk too short")
	}
	a := binary.LittleEndian.Uint32(stored)
	if uint64(a) > uint64(len(stored)-4) {
		return nil, errors.New("shuffle2+huffman chunk: bad record length")
	}
	n := rawLen / 2
	out := make([]byte, rawLen)
	if err := readPlane(stored[4:4+a], n, out, 0, false); err != nil {
		return nil, fmt.Errorf("low plane: %w", err)
	}
	if err := readPlane(stored[4+a:], rawLen-n, out, 1, rawLen%2 == 1); err != nil {
		return nil, fmt.Errorf("high plane: %w", err)
	}
	return out, nil
}
