package store

import (
	"context"
	"fmt"

	"github.com/ai-computing/onggi/pkg/codec"
	"github.com/ai-computing/onggi/pkg/digest"
)

// Compression frames (#52). Two-level chunking splits new chunks into ~4 KiB sub-chunks, and a
// sub-chunk compressed alone compresses badly: switching a node between two decode variants sent
// 28.3 MB where 64 KiB chunks sent 6.1 MB. Push therefore compresses consecutive new chunks of a
// sub-chunked file together, up to Config.Frame raw bytes, as one frame in the pack. Each chunk
// keeps its own index entry, so dedup and the hash check per chunk are unchanged: its Loc names
// the frame (Offset, Stored, Codec), the frame's raw length (Frame) and the chunk's place in it
// (Inner). On the decode B2 → B8 switch, 256 KiB frames send 3.3 MB.

// DefaultFrame is the largest raw size of a compression frame in new two-level stores. It stays
// below 256 KiB so that the byte planes of a frame fit one Huffman block and weights keep the
// Shuffle2Huffman codec (pkg/codec).
const DefaultFrame = 255 << 10

// share is the part of the stored bytes that is the chunk's: for a chunk in a frame, the frame's
// stored bytes split by raw length. GC sums it per pack.
func (l Loc) share() int64 {
	if l.Frame == 0 {
		return int64(l.Stored)
	}
	return int64(l.Stored) * int64(l.Length) / int64(l.Frame)
}

// frameReader decodes each frame once while chunks are read in pack order.
type frameReader struct {
	pack, off uint32
	raw       []byte
	ok        bool
}

// chunk returns the raw bytes of the chunk at l from stored, the chunk's stored bytes or its
// frame's. The result of a framed chunk aliases the decoded frame.
func (fr *frameReader) chunk(stored []byte, l Loc) ([]byte, error) {
	if l.Frame == 0 {
		return codec.Decode(stored, l.Codec, int(l.Length))
	}
	if !fr.ok || fr.pack != l.Pack || fr.off != l.Offset {
		fr.ok = false
		raw, err := codec.Decode(stored, l.Codec, int(l.Frame))
		if err != nil {
			return nil, err
		}
		fr.raw, fr.pack, fr.off, fr.ok = raw, l.Pack, l.Offset, true
	}
	if uint64(l.Inner)+uint64(l.Length) > uint64(len(fr.raw)) {
		return nil, fmt.Errorf("chunk at %d+%d lies outside its frame of %d bytes", l.Inner, l.Length, len(fr.raw))
	}
	return fr.raw[l.Inner : l.Inner+l.Length], nil
}

// alone returns the stored form of a chunk on its own: its stored bytes, or for a chunk in a
// frame its raw bytes encoded again. Node caches and peers hold and send chunks one by one.
func alone(raw, stored []byte, l Loc) ([]byte, codec.ID) {
	if l.Frame == 0 {
		return stored, l.Codec
	}
	data, id := codec.Encode(raw, codec.ModeAuto)
	if id == codec.None {
		data = append([]byte(nil), raw...)
	}
	return data, id
}

// member is one chunk of a frame being built.
type member struct {
	chunk  digest.Digest
	inner  uint32
	length uint32
}

// frameBuilder collects the new chunks of one file and sends them as frames.
type frameBuilder struct {
	limit   int
	mode    codec.Mode
	raw     []byte
	members []member
}

// add appends a new chunk, sending the frame first if the chunk would take it past limit. It
// returns the raw bytes added.
func (fb *frameBuilder) add(ctx context.Context, raw []byte, d digest.Digest, out chan<- pending) (int64, error) {
	if len(fb.raw)+len(raw) > fb.limit {
		if err := fb.flush(ctx, out); err != nil {
			return 0, err
		}
	}
	fb.members = append(fb.members, member{chunk: d, inner: uint32(len(fb.raw)), length: uint32(len(raw))})
	fb.raw = append(fb.raw, raw...)
	return int64(len(raw)), nil
}

// flush sends what the builder holds: one chunk as itself, several as a frame.
func (fb *frameBuilder) flush(ctx context.Context, out chan<- pending) error {
	defer func() { fb.raw, fb.members = nil, nil }()
	switch len(fb.members) {
	case 0:
		return nil
	case 1:
		_, err := send(ctx, fb.raw, fb.members[0].chunk, fb.mode, out)
		return err
	}
	data, id := codec.Encode(fb.raw, fb.mode)
	if id == codec.None {
		data = fb.raw // owned: the builder starts a new buffer
	}
	select {
	case out <- pending{data: data, raw: uint32(len(fb.raw)), codec: id, members: fb.members}:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}
