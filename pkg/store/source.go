package store

import (
	"context"
	"io"
	"sort"

	"github.com/ai-computing/onggi/pkg/backend"
	"github.com/ai-computing/onggi/pkg/codec"
	"github.com/ai-computing/onggi/pkg/digest"
)

// ChunkSource is a place other than the store and the node cache that may hold stored chunks:
// another node's cache, served by pkg/peer (#34). Chunks from a source are checked like any
// other: the pull decodes them and compares their hash.
type ChunkSource interface {
	Name() string
	// Has reports which of ds the source holds.
	Has(ctx context.Context, ds []digest.Digest) ([]bool, error)
	// Fetch calls fn with each of ds the source holds, in stored form, in any order. Chunks it
	// does not hold are skipped; the caller notices them missing.
	Fetch(ctx context.Context, ds []digest.Digest, fn func(d digest.Digest, stored []byte, id codec.ID, rawLen uint32) error) error
}

// HasStored reports which of ds the store holds as stored chunks (bytes in a pack).
func (s *Store) HasStored(ctx context.Context, ds []digest.Digest) ([]bool, error) {
	if err := s.LoadIndex(ctx); err != nil {
		return nil, err
	}
	out := make([]bool, len(ds))
	for i, d := range ds {
		_, e, ok := s.lookup(d)
		out[i] = ok && e.state == stateStored
	}
	return out, nil
}

// ReadStored calls fn with each of ds the store holds as a stored chunk, in stored form,
// reading adjacent chunks of a pack together. Digests it does not hold are skipped.
func (s *Store) ReadStored(ctx context.Context, ds []digest.Digest, fn func(d digest.Digest, stored []byte, id codec.ID, rawLen uint32) error) error {
	if err := s.LoadIndex(ctx); err != nil {
		return err
	}
	type item struct {
		d digest.Digest
		l Loc
	}
	var items []item
	seen := map[digest.Digest]bool{}
	for _, d := range ds {
		if seen[d] {
			continue
		}
		seen[d] = true
		if _, e, ok := s.lookup(d); ok && e.state == stateStored {
			items = append(items, item{d, e.loc})
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].l.Pack != items[j].l.Pack {
			return items[i].l.Pack < items[j].l.Pack
		}
		if items[i].l.Offset != items[j].l.Offset {
			return items[i].l.Offset < items[j].l.Offset
		}
		return items[i].l.Inner < items[j].l.Inner
	})
	const maxRange = 16 << 20
	var buf []byte
	for i := 0; i < len(items); {
		j := i + 1
		for j < len(items) && items[j].l.Pack == items[i].l.Pack &&
			items[j].l.Offset+items[j].l.Stored-items[i].l.Offset <= maxRange &&
			(items[j].l.Offset >= items[j-1].l.Offset+items[j-1].l.Stored || items[j].l.Frame != 0 && items[j].l.Offset == items[j-1].l.Offset) {
			j++
		}
		first, last := items[i].l, items[j-1].l
		n := int64(last.Offset+last.Stored) - int64(first.Offset)
		rc, err := s.B.Get(ctx, backend.Key{Kind: backend.Pack, Name: s.packName(first.Pack).Hex()}, int64(first.Offset), n)
		if err != nil {
			return err
		}
		if int64(cap(buf)) < n {
			buf = make([]byte, n)
		}
		b := buf[:n]
		_, err = io.ReadFull(rc, b)
		rc.Close()
		if err != nil {
			return err
		}
		var fr frameReader
		for _, it := range items[i:j] {
			off := it.l.Offset - first.Offset
			stored := b[off : off+it.l.Stored]
			if it.l.Frame != 0 { // sent on its own: readers decode chunk by chunk (#52)
				raw, err := fr.chunk(stored, it.l)
				if err != nil {
					return err
				}
				data, id := alone(raw, stored, it.l)
				if err := fn(it.d, data, id, it.l.Length); err != nil {
					return err
				}
				continue
			}
			if err := fn(it.d, stored, it.l.Codec, it.l.Length); err != nil {
				return err
			}
		}
		i = j
	}
	return nil
}

// LocalSource serves a store's stored chunks as a ChunkSource without a network: for tests,
// and for a pull that should prefer a second local store.
func LocalSource(name string, s *Store) ChunkSource { return localSource{name, s} }

type localSource struct {
	name string
	s    *Store
}

func (l localSource) Name() string { return l.name }
func (l localSource) Has(ctx context.Context, ds []digest.Digest) ([]bool, error) {
	return l.s.HasStored(ctx, ds)
}
func (l localSource) Fetch(ctx context.Context, ds []digest.Digest, fn func(digest.Digest, []byte, codec.ID, uint32) error) error {
	return l.s.ReadStored(ctx, ds, fn)
}
