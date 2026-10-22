package store

import (
	"bytes"
	"encoding/binary"
	"slices"

	"github.com/ai-computing/onggi/pkg/digest"
)

// chunkIndex is the in-memory index: every chunk the store holds, as stored bytes or as
// sub-chunks. It keeps entries in parallel arrays numbered by id, composites as runs of ids in
// one array, and finds a digest by binary search over ids sorted by digest plus a small map of
// recent additions. A map of digests with per-composite slices cost about 1 GB for the ATOM
// Llama-3.1-8B store; this costs about a third of that (#19).
type chunkIndex struct {
	dig    []digest.Digest
	ent    []entry
	subs   []uint32                 // composite sub-chunks, by id
	sorted []uint32                 // ids whose digests are merged, in digest order
	keys   []uint64                 // the first 8 bytes of sorted's digests, searched instead of the digests
	bucket []uint32                 // bucket[p]: first position in sorted whose digest starts with the 16 bits p
	delta  map[digest.Digest]uint32 // ids added since the last merge
	stored int
	comps  int
}

// entry is one chunk. A sub-chunk named by a composite before its own record is loaded (index
// objects load in any order) has state unknown until that record arrives.
type entry struct {
	loc   Loc // for stored chunks; loc.Length is the raw length for every state
	state uint8
	nsub  uint16
	sub   uint32 // first sub-chunk in subs
}

const (
	stateUnknown = 0
	stateStored  = 1
	stateComp    = 2
)

func newChunkIndex() *chunkIndex { return &chunkIndex{delta: map[digest.Digest]uint32{}} }

func prefix(d *digest.Digest) uint64 { return binary.BigEndian.Uint64(d[:8]) }

// find looks d up. The sorted part is searched in keys, a contiguous array of 8-byte prefixes
// narrowed by bucket, not through the ids into dig: that indirection made loading the ATOM
// store's index take 30 s, most of it in comparisons (#42).
func (x *chunkIndex) find(d digest.Digest) (uint32, bool) {
	if id, ok := x.delta[d]; ok {
		return id, true
	}
	if len(x.sorted) == 0 {
		return 0, false
	}
	k := prefix(&d)
	lo, hi := int(x.bucket[k>>48]), int(x.bucket[k>>48+1])
	for lo < hi { // first position with keys >= k
		mid := int(uint(lo+hi) >> 1)
		if x.keys[mid] < k {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	for i := lo; i < len(x.keys) && x.keys[i] == k; i++ {
		if id := x.sorted[i]; x.dig[id] == d {
			return id, true
		}
	}
	return 0, false
}

// idOf returns the id of d, adding an unknown entry of the given raw length if d is new.
func (x *chunkIndex) idOf(d digest.Digest, length uint32) uint32 {
	if id, ok := x.find(d); ok {
		return id
	}
	id := uint32(len(x.dig))
	x.dig = append(x.dig, d)
	x.ent = append(x.ent, entry{loc: Loc{Length: length}})
	x.delta[d] = id
	if len(x.delta) >= 1<<18+len(x.sorted)/8 {
		x.merge()
	}
	return id
}

// merge folds the recent additions into the sorted ids.
func (x *chunkIndex) merge() {
	add := make([]uint32, 0, len(x.delta))
	for _, id := range x.delta {
		add = append(add, id)
	}
	cmp := func(a, b uint32) int {
		if pa, pb := prefix(&x.dig[a]), prefix(&x.dig[b]); pa != pb {
			if pa < pb {
				return -1
			}
			return 1
		}
		return bytes.Compare(x.dig[a][:], x.dig[b][:])
	}
	slices.SortFunc(add, cmp)
	out := make([]uint32, 0, len(x.sorted)+len(add))
	i, j := 0, 0
	for i < len(x.sorted) && j < len(add) {
		if cmp(x.sorted[i], add[j]) < 0 {
			out = append(out, x.sorted[i])
			i++
		} else {
			out = append(out, add[j])
			j++
		}
	}
	out = append(append(out, x.sorted[i:]...), add[j:]...)
	x.sorted = out
	x.keys = slices.Grow(x.keys[:0], len(out))[:len(out)]
	if x.bucket == nil {
		x.bucket = make([]uint32, 1<<16+1)
	}
	clear(x.bucket)
	for i, id := range out {
		k := prefix(&x.dig[id])
		x.keys[i] = k
		x.bucket[k>>48+1]++
	}
	for p := 1; p < len(x.bucket); p++ {
		x.bucket[p] += x.bucket[p-1]
	}
	clear(x.delta)
}

// addStored records a chunk held as bytes. A chunk already stored keeps its first location;
// one known only as a composite becomes stored (a stored chunk is read whole).
func (x *chunkIndex) addStored(d digest.Digest, loc Loc) {
	id := x.idOf(d, loc.Length)
	e := &x.ent[id]
	switch e.state {
	case stateStored:
		return
	case stateComp:
		x.comps--
	}
	e.loc, e.state, e.nsub = loc, stateStored, 0
	x.stored++
}

// addComposite records a chunk held as sub-chunks, unless it is already known.
func (x *chunkIndex) addComposite(c composite) {
	var length uint32
	for _, s := range c.Subs {
		length += s.Length
	}
	id := x.idOf(c.Chunk, length)
	if x.ent[id].state != stateUnknown {
		return
	}
	start := uint32(len(x.subs))
	for _, s := range c.Subs {
		x.subs = append(x.subs, x.idOf(s.Digest, s.Length)) // may append entries: index again below
	}
	e := &x.ent[id]
	e.state, e.sub, e.nsub = stateComp, start, uint16(len(c.Subs))
	e.loc.Length = length
	x.comps++
}

func (x *chunkIndex) subsOf(e entry) []uint32 { return x.subs[e.sub : e.sub+uint32(e.nsub)] }

// ref describes entry id as a chunk reference.
func (x *chunkIndex) ref(id uint32) ChunkRef {
	return ChunkRef{Digest: x.dig[id], Length: x.ent[id].loc.Length}
}
