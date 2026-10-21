// Package codec compresses single chunks after dedup.
//
// Chunk digests are always taken over the raw bytes, so a chunk deduplicates the same way however
// it is stored. Codecs:
//
//   - None: raw bytes.
//   - Zstd: Zstandard (RFC 8878).
//   - Shuffle2Zstd: the bytes of each 2-byte word split into two planes (low bytes, high bytes),
//     each plane a separate Zstandard frame: uint32 length of the first frame, the first frame, the
//     second frame. Separate frames matter: within one frame the planes share a literal table and
//     the near-random low plane hides the redundancy of the high plane. The split is the
//     byte-shuffle filter of HDF5 and Blosc, also used per chunk by Hugging Face Xet (4-byte
//     groups there); on bf16/fp16 weights the high plane (sign and exponent) compresses well and
//     the low plane hardly at all (docs/related-work.md §2).
//
// Auto tries Zstd and Shuffle2Zstd and keeps the smaller; a chunk that does not shrink by at least
// 3 % is stored raw.
package codec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// ID names a codec in the pack index. Values are stored; never renumber.
type ID uint8

const (
	None         ID = 0
	Zstd         ID = 1
	Shuffle2Zstd ID = 2
)

func (id ID) String() string {
	switch id {
	case None:
		return "none"
	case Zstd:
		return "zstd"
	case Shuffle2Zstd:
		return "shuffle2+zstd"
	}
	return fmt.Sprintf("codec(%d)", uint8(id))
}

// Mode selects how Encode picks a codec.
type Mode string

const (
	ModeOff  Mode = "none" // store raw
	ModeZstd Mode = "zstd" // Zstd only
	ModeAuto Mode = "auto" // Zstd or Shuffle2Zstd, whichever is smaller
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

var (
	encOnce sync.Once
	enc     *zstd.Encoder
	dec     *zstd.Decoder
)

func coders() (*zstd.Encoder, *zstd.Decoder) {
	encOnce.Do(func() {
		// EncodeAll/DecodeAll are safe for concurrent use; one coder pair serves all workers.
		enc, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1),
			zstd.WithZeroFrames(true))
		dec, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(0), zstd.WithDecoderMaxMemory(1<<30))
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
	if m == ModeAuto && len(raw) >= 4 {
		sh := encodePlanes(e, raw)
		if len(sh) < len(best) {
			best, id = sh, Shuffle2Zstd
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
	case Shuffle2Zstd:
		return decodePlanes(stored, rawLen)
	}
	return nil, errors.New("unknown codec " + id.String())
}

// encodePlanes stores the low plane (even-index bytes) and the high plane (odd-index bytes plus an
// odd trailing byte) as two Zstandard frames.
func encodePlanes(e *zstd.Encoder, raw []byte) []byte {
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
	out := make([]byte, 4, 4+len(raw)/2)
	out = e.EncodeAll(lo, out)
	binary.LittleEndian.PutUint32(out, uint32(len(out)-4))
	return e.EncodeAll(hi, out)
}

func decodePlanes(stored []byte, rawLen int) ([]byte, error) {
	if len(stored) < 4 {
		return nil, errors.New("shuffle2+zstd chunk too short")
	}
	a := binary.LittleEndian.Uint32(stored)
	if uint64(a) > uint64(len(stored)-4) {
		return nil, errors.New("shuffle2+zstd chunk: bad frame length")
	}
	_, d := coders()
	n := rawLen / 2
	lo, err := d.DecodeAll(stored[4:4+a], make([]byte, 0, n))
	if err != nil {
		return nil, err
	}
	hi, err := d.DecodeAll(stored[4+a:], make([]byte, 0, rawLen-n))
	if err != nil {
		return nil, err
	}
	if len(lo) != n || len(hi) != rawLen-n {
		return nil, fmt.Errorf("shuffle2+zstd chunk decodes to %d+%d bytes, want %d", len(lo), len(hi), rawLen)
	}
	out := make([]byte, rawLen)
	for i := 0; i < n; i++ {
		out[2*i] = lo[i]
		out[2*i+1] = hi[i]
	}
	if rawLen%2 == 1 {
		out[rawLen-1] = hi[n]
	}
	return out, nil
}
