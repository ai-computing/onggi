package store

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"

	"github.com/ai-computing/onggi/pkg/codec"
	"github.com/ai-computing/onggi/pkg/digest"
)

// A locator lists everything a pull of one artifact needs: every chunk it reaches (stored chunks
// with their place in a pack, composites with their sub-chunks by node number), the packs it
// reads and each file's chunks by node number. Push writes it before the manifest, which names
// it, so it is checked through the manifest digest. With it, pull reads neither the store's
// whole index nor the chunk lists, and plans without a map by digest (#17).
//
// Nodes are in post-order: a composite comes after all its sub-chunks, so push writes each node
// as soon as it is known and pull links each composite to nodes it has already read.
//
//	magic "ONGGILC1"
//	nodes  uint32, then per node: chunk digest [32], raw length uint32, kind uint8, then
//	       kind 0 (stored):    pack number uint32, offset uint32, stored length uint32, codec uint8
//	       kind 1 (composite): sub-chunk count uint32, then that many earlier node numbers uint32
//	packs  uint32, then that many pack digests [32]
//	files  uint32, then per file: chunk count uint32, then that many node numbers uint32

var locatorMagic = [8]byte{'O', 'N', 'G', 'G', 'I', 'L', 'C', '1'}

// writeLocator writes the locator of files whose chunk lists are lists to f, from its start,
// and returns its size. The store's index must be loaded and hold every chunk. Writing to a file
// keeps a locator of hundreds of MB out of memory (#17).
func (s *Store) writeLocator(lists [][]ChunkRef, f *os.File) (int64, error) {
	b := bufio.NewWriterSize(f, 1<<20)
	var size int64
	var tmp [4]byte
	w := func(v uint32) {
		binary.LittleEndian.PutUint32(tmp[:], v)
		b.Write(tmp[:])
		size += 4
	}
	raw := func(p []byte) {
		b.Write(p)
		size += int64(len(p))
	}
	one := func(c byte) {
		b.WriteByte(c)
		size++
	}
	raw(locatorMagic[:])
	w(0) // node count, set at the end
	var nodes uint32
	nodeOf := map[digest.Digest]uint32{}
	packOf := map[uint32]uint32{} // store pack number -> locator pack number
	var packs []digest.Digest
	var add func(c ChunkRef) (uint32, error)
	add = func(c ChunkRef) (uint32, error) {
		if id, ok := nodeOf[c.Digest]; ok {
			return id, nil
		}
		s.mu.RLock()
		loc, stored := s.index[c.Digest]
		subs, comp := s.comps[c.Digest]
		s.mu.RUnlock()
		switch {
		case stored:
			p, ok := packOf[loc.Pack]
			if !ok {
				p = uint32(len(packs))
				packOf[loc.Pack] = p
				packs = append(packs, s.packName(loc.Pack))
			}
			raw(c.Digest[:])
			w(c.Length)
			one(0)
			w(p)
			w(loc.Offset)
			w(loc.Stored)
			one(byte(loc.Codec))
		case comp:
			kids := make([]uint32, len(subs))
			for i, sc := range subs {
				k, err := add(sc)
				if err != nil {
					return 0, err
				}
				kids[i] = k
			}
			raw(c.Digest[:])
			w(c.Length)
			one(1)
			w(uint32(len(kids)))
			for _, k := range kids {
				w(k)
			}
		default:
			return 0, fmt.Errorf("chunk %s is not in the store", c.Digest)
		}
		id := nodes
		nodes++
		nodeOf[c.Digest] = id
		return id, nil
	}
	files := make([][]uint32, len(lists))
	for i, list := range lists {
		files[i] = make([]uint32, len(list))
		for j, c := range list {
			id, err := add(c)
			if err != nil {
				return 0, err
			}
			files[i][j] = id
		}
	}
	nodeOf = nil
	w(uint32(len(packs)))
	for _, p := range packs {
		raw(p[:])
	}
	w(uint32(len(files)))
	for _, f := range files {
		w(uint32(len(f)))
		for _, id := range f {
			w(id)
		}
	}
	if err := b.Flush(); err != nil {
		return 0, err
	}
	binary.LittleEndian.PutUint32(tmp[:], nodes)
	if _, err := f.WriteAt(tmp[:], 8); err != nil {
		return 0, err
	}
	return size, nil
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

// readLocator decodes a locator straight into a pull plan for the files of m: the nodes with
// their parent edges and targets, and the packs that node locations number.
func readLocator(data []byte, m *Manifest) ([]node, []digest.Digest, error) {
	if len(data) < 8 || !bytes.Equal(data[:8], locatorMagic[:]) {
		return nil, nil, errors.New("not an onggi locator")
	}
	r := &reader{p: data[8:]}
	pl := &planner{nodes: make([]node, r.count(41))}
	lengths := make([]uint32, len(pl.nodes))
	for i := range pl.nodes {
		nd := &pl.nodes[i]
		copy(nd.chunk[:], r.take(32))
		length := r.u32()
		lengths[i] = length
		switch r.u8() {
		case 0:
			nd.stored = true
			nd.loc = Loc{Pack: r.u32(), Offset: r.u32(), Stored: r.u32(), Length: length}
			nd.loc.Codec = codec.ID(r.u8())
		case 1:
			var off int64
			for j, n := 0, r.count(4); j < n; j++ {
				k := r.u32()
				if r.err != nil {
					break
				}
				if int(k) >= i || lengths[k] >= length { // post-order: sub-chunks come first and are shorter
					return nil, nil, fmt.Errorf("locator: chunk %s has a bad sub-chunk", nd.chunk)
				}
				pl.addParent(int32(k), edge{node: int32(i), off: uint32(off)})
				off += int64(lengths[k])
			}
			if r.err == nil && off != int64(length) {
				return nil, nil, fmt.Errorf("locator: chunk %s: sub-chunks add up to %d bytes, want %d", nd.chunk, off, length)
			}
		default:
			if r.err == nil {
				return nil, nil, fmt.Errorf("locator: node %d has an unknown kind", i)
			}
		}
		if r.err != nil {
			return nil, nil, r.err
		}
	}
	packs := make([]digest.Digest, r.count(32))
	for i := range packs {
		copy(packs[i][:], r.take(32))
	}
	for i := range pl.nodes {
		if pl.nodes[i].stored && int(pl.nodes[i].loc.Pack) >= len(packs) {
			return nil, nil, fmt.Errorf("locator: node %d names pack %d of %d", i, pl.nodes[i].loc.Pack, len(packs))
		}
	}
	if n := r.count(4); r.err == nil && n != len(m.Files) {
		return nil, nil, fmt.Errorf("locator: %d files, manifest has %d", n, len(m.Files))
	}
	for i, f := range m.Files {
		var off int64
		for j, n := 0, r.count(4); j < n; j++ {
			id := r.u32()
			if r.err != nil {
				break
			}
			if int(id) >= len(pl.nodes) {
				return nil, nil, fmt.Errorf("locator: %s names node %d of %d", f.Path, id, len(pl.nodes))
			}
			pl.addTarget(int32(id), target{file: int32(i), off: off})
			off += int64(lengths[id])
		}
		if r.err == nil && off != f.Size {
			return nil, nil, fmt.Errorf("%s: chunks add up to %d bytes, manifest says %d", f.Path, off, f.Size)
		}
	}
	if r.err != nil {
		return nil, nil, r.err
	}
	if len(r.p) != 0 {
		return nil, nil, fmt.Errorf("locator: %d trailing bytes", len(r.p))
	}
	return pl.nodes, packs, nil
}
