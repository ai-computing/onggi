package store

import (
	"encoding/binary"
	"testing"

	"github.com/ai-computing/onggi/pkg/digest"
)

func dg(i int) digest.Digest {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(i))
	return digest.Of(b[:])
}

// Lookups stay right before, across and after merges of the recent additions.
func TestChunkIndexFindsAcrossMerges(t *testing.T) {
	x := newChunkIndex()
	for i := 0; i < 5000; i++ {
		x.addStored(dg(i), Loc{Offset: uint32(i), Length: 10})
		if i%997 == 0 {
			x.merge()
		}
	}
	if x.stored != 5000 || len(x.sorted)+len(x.delta) != 5000 {
		t.Fatalf("stored %d, sorted %d + delta %d", x.stored, len(x.sorted), len(x.delta))
	}
	for i := 0; i < 5000; i++ {
		id, ok := x.find(dg(i))
		if !ok || x.ent[id].loc.Offset != uint32(i) {
			t.Fatalf("chunk %d: found %v, offset %d", i, ok, x.ent[id].loc.Offset)
		}
	}
	x.merge()
	for _, i := range []int{0, 1, 2500, 4999} {
		if _, ok := x.find(dg(i)); !ok {
			t.Fatalf("chunk %d lost in the merge", i)
		}
	}
	if _, ok := x.find(dg(5000)); ok {
		t.Fatal("found a chunk never added")
	}
	// re-adding keeps the first location
	x.addStored(dg(7), Loc{Offset: 99, Length: 10})
	if id, _ := x.find(dg(7)); x.ent[id].loc.Offset != 7 || x.stored != 5000 {
		t.Fatal("a second record replaced the first")
	}
}

// Index objects load in any order: a composite may name sub-chunks whose pack index comes later.
func TestChunkIndexCompositeBeforeItsSubChunks(t *testing.T) {
	x := newChunkIndex()
	c := composite{Chunk: dg(100), Subs: []ChunkRef{{dg(1), 4096}, {dg(2), 1000}}}
	x.addComposite(c)
	id, _ := x.find(dg(100))
	if x.ent[id].state != stateComp || x.ent[id].loc.Length != 5096 || x.comps != 1 {
		t.Fatalf("composite entry %+v", x.ent[id])
	}
	if sid, ok := x.find(dg(1)); !ok || x.ent[sid].state != stateUnknown {
		t.Fatal("sub-chunk should be known by digest but not yet producible")
	}
	x.addStored(dg(1), Loc{Offset: 5, Length: 4096})
	x.addStored(dg(2), Loc{Offset: 6, Length: 1000})
	subs := x.subsOf(x.ent[id])
	if len(subs) != 2 || x.ref(subs[0]) != c.Subs[0] || x.ent[subs[1]].state != stateStored {
		t.Fatalf("sub-chunks %v", subs)
	}
	// a stored record for the composite's own bytes wins
	x.addStored(dg(100), Loc{Offset: 9, Length: 5096})
	if x.ent[id].state != stateStored || x.comps != 0 || x.stored != 3 {
		t.Fatalf("stored record did not replace the composite: %+v", x.ent[id])
	}
	// a composite record for a stored chunk changes nothing
	x.addComposite(composite{Chunk: dg(1), Subs: []ChunkRef{{dg(3), 2048}, {dg(4), 2048}}})
	if sid, _ := x.find(dg(1)); x.ent[sid].state != stateStored {
		t.Fatal("composite record replaced a stored chunk")
	}
}

// Digests that share their first 8 bytes, or their first 16 bits, are still told apart (#42).
func TestIndexFindWithSharedPrefixes(t *testing.T) {
	x := newChunkIndex()
	var ds []digest.Digest
	for i := 0; i < 3000; i++ {
		var d digest.Digest
		d[0], d[1] = 0xab, byte(i%3) // three 16-bit buckets
		if i%2 == 0 {
			d[31] = byte(i) // same 8-byte prefix as its neighbours
			d[30] = byte(i >> 8)
		} else {
			d[7] = byte(i)
			d[6] = byte(i >> 8)
		}
		ds = append(ds, d)
		x.addStored(d, Loc{Length: uint32(i + 1)})
		if i == 1500 {
			x.merge()
		}
	}
	x.merge()
	for i, d := range ds {
		id, ok := x.find(d)
		if !ok || x.ent[id].loc.Length != uint32(i+1) {
			t.Fatalf("digest %d: found %v, id %d", i, ok, id)
		}
	}
	var none digest.Digest
	none[0], none[1] = 0xab, 1
	none[5] = 0xff
	if _, ok := x.find(none); ok {
		t.Fatal("found a digest that was never added")
	}
	none[0] = 0xff
	if _, ok := x.find(none); ok {
		t.Fatal("found a digest past the last bucket")
	}
}
