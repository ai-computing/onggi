package store

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/ai-computing/onggi/pkg/codec"
	"github.com/ai-computing/onggi/pkg/digest"
)

// A locator lists everything a pull of one artifact needs: the packs it reads, every chunk it
// reaches (stored chunks with their place in a pack, composites with their sub-chunks by node
// number) and each file's chunks by node number. Push writes it before the manifest, which
// names it, so it is checked through the manifest digest. With it, pull reads neither the
// store's whole index nor the chunk lists, and plans without a map by digest (#17).
//
//	magic "ONGGILC1"
//	packs  uint32, then that many pack digests [32]
//	nodes  uint32, then per node: chunk digest [32], raw length uint32, kind uint8, then
//	       kind 0 (stored):    pack number uint32, offset uint32, stored length uint32, codec uint8
//	       kind 1 (composite): sub-chunk count uint32, then that many node numbers uint32
//	files  uint32, then per file: chunk count uint32, then that many node numbers uint32

var locatorMagic = [8]byte{'O', 'N', 'G', 'G', 'I', 'L', 'C', '1'}

type locNode struct {
	Chunk  digest.Digest
	Length uint32
	Comp   bool
	Loc    Loc      // stored: Pack numbers Locator.Packs
	Subs   []uint32 // composite
}

// Locator is the decoded form.
type Locator struct {
	Packs []digest.Digest
	Nodes []locNode
	Files [][]uint32
}

func (l *Locator) encode() []byte {
	var b bytes.Buffer
	w := func(v uint32) { binary.Write(&b, binary.LittleEndian, v) }
	b.Write(locatorMagic[:])
	w(uint32(len(l.Packs)))
	for _, p := range l.Packs {
		b.Write(p[:])
	}
	w(uint32(len(l.Nodes)))
	for _, n := range l.Nodes {
		b.Write(n.Chunk[:])
		w(n.Length)
		if n.Comp {
			b.WriteByte(1)
			w(uint32(len(n.Subs)))
			for _, s := range n.Subs {
				w(s)
			}
			continue
		}
		b.WriteByte(0)
		w(n.Loc.Pack)
		w(n.Loc.Offset)
		w(n.Loc.Stored)
		b.WriteByte(byte(n.Loc.Codec))
	}
	w(uint32(len(l.Files)))
	for _, f := range l.Files {
		w(uint32(len(f)))
		for _, id := range f {
			w(id)
		}
	}
	return b.Bytes()
}

type reader struct {
	p   []byte
	err error
}

func (r *reader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if len(r.p) < n {
		r.err = errors.New("locator truncated")
		return nil
	}
	b := r.p[:n]
	r.p = r.p[n:]
	return b
}

func (r *reader) u32() uint32 {
	if b := r.take(4); b != nil {
		return binary.LittleEndian.Uint32(b)
	}
	return 0
}

func (r *reader) u8() uint8 {
	if b := r.take(1); b != nil {
		return b[0]
	}
	return 0
}

// count reads a count of items of at least min bytes each, refusing counts the rest of the data
// cannot hold (so a damaged count cannot make us allocate gigabytes).
func (r *reader) count(min int) int {
	n := r.u32()
	if r.err == nil && uint64(n)*uint64(min) > uint64(len(r.p)) {
		r.err = errors.New("locator: count exceeds the data")
	}
	return int(n)
}

func decodeLocator(data []byte) (*Locator, error) {
	if len(data) < 8 || !bytes.Equal(data[:8], locatorMagic[:]) {
		return nil, errors.New("not an onggi locator")
	}
	r := &reader{p: data[8:]}
	l := &Locator{}
	l.Packs = make([]digest.Digest, r.count(32))
	for i := range l.Packs {
		copy(l.Packs[i][:], r.take(32))
	}
	l.Nodes = make([]locNode, r.count(37))
	for i := range l.Nodes {
		n := &l.Nodes[i]
		copy(n.Chunk[:], r.take(32))
		n.Length = r.u32()
		switch r.u8() {
		case 0:
			n.Loc = Loc{Pack: r.u32(), Offset: r.u32(), Stored: r.u32(), Length: n.Length}
			n.Loc.Codec = codec.ID(r.u8())
			if r.err == nil && int(n.Loc.Pack) >= len(l.Packs) {
				return nil, fmt.Errorf("locator: node %d names pack %d of %d", i, n.Loc.Pack, len(l.Packs))
			}
		case 1:
			n.Comp = true
			n.Subs = make([]uint32, r.count(4))
			for j := range n.Subs {
				n.Subs[j] = r.u32()
			}
		default:
			if r.err == nil {
				return nil, fmt.Errorf("locator: node %d has an unknown kind", i)
			}
		}
	}
	l.Files = make([][]uint32, r.count(4))
	for i := range l.Files {
		l.Files[i] = make([]uint32, r.count(4))
		for j := range l.Files[i] {
			l.Files[i][j] = r.u32()
		}
	}
	if r.err != nil {
		return nil, r.err
	}
	if len(r.p) != 0 {
		return nil, fmt.Errorf("locator: %d trailing bytes", len(r.p))
	}
	return l, nil
}

// buildLocator collects the locator of files whose chunk lists are lists. The store's index must
// be loaded and hold every chunk.
func (s *Store) buildLocator(lists [][]ChunkRef) (*Locator, error) {
	l := &Locator{}
	nodeOf := map[digest.Digest]uint32{}
	packOf := map[uint32]uint32{} // store pack number -> locator pack number
	var add func(c ChunkRef) (uint32, error)
	add = func(c ChunkRef) (uint32, error) {
		if id, ok := nodeOf[c.Digest]; ok {
			return id, nil
		}
		s.mu.RLock()
		loc, stored := s.index[c.Digest]
		subs, comp := s.comps[c.Digest]
		s.mu.RUnlock()
		id := uint32(len(l.Nodes))
		switch {
		case stored:
			p, ok := packOf[loc.Pack]
			if !ok {
				p = uint32(len(l.Packs))
				packOf[loc.Pack] = p
				l.Packs = append(l.Packs, s.packName(loc.Pack))
			}
			loc.Pack = p
			nodeOf[c.Digest] = id
			l.Nodes = append(l.Nodes, locNode{Chunk: c.Digest, Length: c.Length, Loc: loc})
		case comp:
			nodeOf[c.Digest] = id
			l.Nodes = append(l.Nodes, locNode{Chunk: c.Digest, Length: c.Length, Comp: true})
			kids := make([]uint32, len(subs))
			for i, sc := range subs {
				k, err := add(sc)
				if err != nil {
					return 0, err
				}
				kids[i] = k
			}
			l.Nodes[id].Subs = kids
		default:
			return 0, fmt.Errorf("chunk %s is not in the store", c.Digest)
		}
		return id, nil
	}
	for _, list := range lists {
		ids := make([]uint32, len(list))
		for i, c := range list {
			id, err := add(c)
			if err != nil {
				return nil, err
			}
			ids[i] = id
		}
		l.Files = append(l.Files, ids)
	}
	return l, nil
}
