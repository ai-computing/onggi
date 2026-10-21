package store

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/ai-computing/onggi/pkg/chunker"
	"github.com/ai-computing/onggi/pkg/codec"
	"github.com/ai-computing/onggi/pkg/digest"
	"github.com/ai-computing/onggi/pkg/layout"
)

// On-disk formats. Binary objects start with an 8-byte magic that names the format and version.
//
//	index  (one per pack): magic "ONGGIIX2", pack digest [32], count uint32, then count ×
//	                       (chunk digest [32], offset uint32, stored length uint32,
//	                        raw length uint32, codec uint8, 3 zero bytes)
//	                       Version 1 ("ONGGIIX1", entries of digest, offset, length; every chunk
//	                       raw) is still read.
//	list   (one per file): magic "ONGGICL1", count uint32, then count × (chunk digest [32], length uint32)
//	composites (index kind, one per push that made any): magic "ONGGICX1", count uint32, then count ×
//	                       (chunk digest [32], sub-chunk count uint32, then that many
//	                        (sub-chunk digest [32], length uint32))
//
// A composite is a chunk stored as its sub-chunks (two-level chunking, #15). Its digest is still
// the digest of its raw bytes, so a list may name it like any other chunk.
// Integers are little-endian. Manifests and the store config are JSON.

var (
	indexMagic   = [8]byte{'O', 'N', 'G', 'G', 'I', 'I', 'X', '2'}
	indexMagicV1 = [8]byte{'O', 'N', 'G', 'G', 'I', 'I', 'X', '1'}
	listMagic    = [8]byte{'O', 'N', 'G', 'G', 'I', 'C', 'L', '1'}
	compMagic    = [8]byte{'O', 'N', 'G', 'G', 'I', 'C', 'X', '1'}
)

// Loc is where a chunk's bytes lie and how they are stored.
type Loc struct {
	Pack   digest.Digest
	Offset uint32
	Stored uint32 // bytes in the pack
	Length uint32 // raw bytes
	Codec  codec.ID
}

type indexEntry struct {
	Chunk digest.Digest
	Loc   Loc
}

func encodeIndex(pack digest.Digest, entries []indexEntry) []byte {
	var b bytes.Buffer
	b.Write(indexMagic[:])
	b.Write(pack[:])
	binary.Write(&b, binary.LittleEndian, uint32(len(entries)))
	for _, e := range entries {
		b.Write(e.Chunk[:])
		binary.Write(&b, binary.LittleEndian, e.Loc.Offset)
		binary.Write(&b, binary.LittleEndian, e.Loc.Stored)
		binary.Write(&b, binary.LittleEndian, e.Loc.Length)
		b.Write([]byte{byte(e.Loc.Codec), 0, 0, 0})
	}
	return b.Bytes()
}

func decodeIndex(data []byte) (digest.Digest, []indexEntry, error) {
	var pack digest.Digest
	if len(data) < 44 {
		return pack, nil, errors.New("not an onggi index")
	}
	v1 := bytes.Equal(data[:8], indexMagicV1[:])
	if !v1 && !bytes.Equal(data[:8], indexMagic[:]) {
		return pack, nil, errors.New("not an onggi index")
	}
	size := uint64(48)
	if v1 {
		size = 40
	}
	copy(pack[:], data[8:40])
	n := binary.LittleEndian.Uint32(data[40:44])
	if uint64(len(data)) != 44+uint64(n)*size {
		return pack, nil, fmt.Errorf("index: %d entries but %d bytes", n, len(data))
	}
	out := make([]indexEntry, n)
	p := data[44:]
	for i := range out {
		copy(out[i].Chunk[:], p[:32])
		l := Loc{Pack: pack, Offset: binary.LittleEndian.Uint32(p[32:36]), Stored: binary.LittleEndian.Uint32(p[36:40])}
		if v1 {
			l.Length = l.Stored
		} else {
			l.Length = binary.LittleEndian.Uint32(p[40:44])
			l.Codec = codec.ID(p[44])
		}
		out[i].Loc = l
		p = p[size:]
	}
	return pack, out, nil
}

// ChunkRef is one chunk of a file.
type ChunkRef struct {
	Digest digest.Digest
	Length uint32
}

func encodeList(chunks []ChunkRef) []byte {
	b := make([]byte, 12, 12+36*len(chunks))
	copy(b, listMagic[:])
	binary.LittleEndian.PutUint32(b[8:], uint32(len(chunks)))
	var tmp [4]byte
	for _, c := range chunks {
		b = append(b, c.Digest[:]...)
		binary.LittleEndian.PutUint32(tmp[:], c.Length)
		b = append(b, tmp[:]...)
	}
	return b
}

func decodeList(data []byte) ([]ChunkRef, error) {
	if len(data) < 12 || !bytes.Equal(data[:8], listMagic[:]) {
		return nil, errors.New("not an onggi chunk list")
	}
	n := binary.LittleEndian.Uint32(data[8:12])
	if uint64(len(data)) != 12+uint64(n)*36 {
		return nil, fmt.Errorf("chunk list: %d entries but %d bytes", n, len(data))
	}
	out := make([]ChunkRef, n)
	p := data[12:]
	for i := range out {
		copy(out[i].Digest[:], p[:32])
		out[i].Length = binary.LittleEndian.Uint32(p[32:36])
		p = p[36:]
	}
	return out, nil
}

// composite maps a chunk to the sub-chunks that hold its bytes, in order.
type composite struct {
	Chunk digest.Digest
	Subs  []ChunkRef
}

func encodeComposites(cs []composite) []byte {
	var b bytes.Buffer
	b.Write(compMagic[:])
	binary.Write(&b, binary.LittleEndian, uint32(len(cs)))
	for _, c := range cs {
		b.Write(c.Chunk[:])
		binary.Write(&b, binary.LittleEndian, uint32(len(c.Subs)))
		for _, s := range c.Subs {
			b.Write(s.Digest[:])
			binary.Write(&b, binary.LittleEndian, s.Length)
		}
	}
	return b.Bytes()
}

func decodeComposites(data []byte) ([]composite, error) {
	if len(data) < 12 || !bytes.Equal(data[:8], compMagic[:]) {
		return nil, errors.New("not an onggi composite table")
	}
	n := binary.LittleEndian.Uint32(data[8:12])
	p := data[12:]
	out := make([]composite, 0, n)
	for i := uint32(0); i < n; i++ {
		if len(p) < 36 {
			return nil, errors.New("composite table truncated")
		}
		var c composite
		copy(c.Chunk[:], p[:32])
		k := binary.LittleEndian.Uint32(p[32:36])
		p = p[36:]
		if uint64(len(p)) < uint64(k)*36 {
			return nil, errors.New("composite table truncated")
		}
		c.Subs = make([]ChunkRef, k)
		for j := range c.Subs {
			copy(c.Subs[j].Digest[:], p[:32])
			c.Subs[j].Length = binary.LittleEndian.Uint32(p[32:36])
			p = p[36:]
		}
		out = append(out, c)
	}
	if len(p) != 0 {
		return nil, fmt.Errorf("composite table: %d trailing bytes", len(p))
	}
	return out, nil
}

// Config is written once when a store is created. Every push to the store uses its chunk
// parameters, or chunks would never match across pushes.
type Config struct {
	Schema   string         `json:"schema"` // "onggi.store.v1"
	Chunker  chunker.Params `json:"chunker"`
	PackSize int64          `json:"pack_size"`
	// Sub, when set, makes push split every chunk the store lacks into content-defined
	// sub-chunks (two-level chunking, #15). Stores created before it have none.
	Sub *chunker.Params `json:"sub_chunker,omitempty"`
	// SubKinds limits sub-chunking to these file kinds (by name, pkg/layout); empty means all.
	// Weights that equal their HF source dedup whole at 64 KiB, and finer chunks only add metadata.
	SubKinds []layout.Kind `json:"sub_kinds,omitempty"`
	Created  time.Time     `json:"created"`
}

// File is one file of an artifact.
type File struct {
	Path string        `json:"path"` // slash-separated, relative to the artifact root
	Size int64         `json:"size"`
	Mode uint32        `json:"mode"`
	Kind layout.Kind   `json:"kind"`
	List digest.Digest `json:"list"` // chunk-list object; it identifies the content
}

// Manifest is one artifact. Its digest is the artifact's identity.
type Manifest struct {
	Schema  string            `json:"schema"` // "onggi.manifest.v1"
	Created time.Time         `json:"created"`
	Chunker chunker.Params    `json:"chunker"`
	Bytes   int64             `json:"bytes"`
	Files   []File            `json:"files"`
	Labels  map[string]string `json:"labels,omitempty"`
}
