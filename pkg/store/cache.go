package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"hash"
	"os"
	"sync"

	"github.com/ai-computing/onggi/pkg/backend"
	"github.com/ai-computing/onggi/pkg/codec"
	"github.com/ai-computing/onggi/pkg/digest"
)

// packWriter adds chunks to a store outside a push: a pull fills its node cache with it (#23).
// Chunks arrive in stored form (already compressed, hash-checked by the caller) and are packed
// as they are, so nothing is decoded or compressed again. A full pack is uploaded with its
// index at once, which makes its chunks visible to the next pull.
type packWriter struct {
	s   *Store
	dir string

	mu      sync.Mutex
	f       *os.File
	h       hash.Hash
	size    int64
	entries []indexEntry
	seen    map[digest.Digest]bool
	bytes   int64 // stored bytes added
	force   bool  // add chunks the store already holds (GC repack copies them out of a pack)
}

func (s *Store) newPackWriter(dir string) *packWriter {
	if dir == "" {
		dir = os.TempDir()
	}
	return &packWriter{s: s, dir: dir, seen: map[digest.Digest]bool{}}
}

// add packs one chunk unless the store or this writer already has it.
func (w *packWriter) add(ctx context.Context, d digest.Digest, stored []byte, id codec.ID, rawLen uint32) error {
	if !w.force && w.s.has(d) {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.seen[d] {
		return nil
	}
	if w.f == nil {
		f, err := os.CreateTemp(w.dir, "onggi-cache-pack-*")
		if err != nil {
			return err
		}
		w.f, w.h = f, sha256.New()
	}
	if _, err := w.f.Write(stored); err != nil {
		return err
	}
	w.h.Write(stored)
	w.entries = append(w.entries, indexEntry{Chunk: d, Loc: Loc{Offset: uint32(w.size), Stored: uint32(len(stored)), Length: rawLen, Codec: id}})
	w.size += int64(len(stored))
	w.bytes += int64(len(stored))
	w.seen[d] = true
	if w.size >= w.s.Config.PackSize {
		return w.seal(ctx)
	}
	return nil
}

// seal uploads the current pack and its index. The caller holds mu.
func (w *packWriter) seal(ctx context.Context) error {
	if w.f == nil {
		return nil
	}
	f := w.f
	w.f = nil
	defer os.Remove(f.Name())
	if err := f.Close(); err != nil {
		return err
	}
	var d digest.Digest
	copy(d[:], w.h.Sum(nil))
	err := w.s.upload(ctx, sealed{path: f.Name(), digest: d, size: w.size, entries: w.entries})
	w.size, w.entries = 0, nil
	return err
}

// close uploads what is left; abort drops it.
func (w *packWriter) close(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seal(ctx)
}

func (w *packWriter) abort() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f != nil {
		w.f.Close()
		os.Remove(w.f.Name())
		w.f = nil
	}
}

// locate marks the nodes in need whose chunks this store holds as stored bytes: fromCache, with
// their location here. It returns the packs those locations index. When few chunks are wanted, as
// after delta apply, it reads the pack indexes without building the store's index, which for a
// node cache of 3.3 M chunks took most of a 15 s pull (#42).
func (s *Store) locate(ctx context.Context, nodes []node, need []int32) ([]digest.Digest, error) {
	s.loadMu.Lock()
	loaded := s.loaded
	s.loadMu.Unlock()
	if loaded || len(need)*16 >= len(nodes) {
		if err := s.LoadIndex(ctx); err != nil {
			return nil, err
		}
		for _, i := range need {
			nd := &nodes[i]
			if _, e, ok := s.lookup(nd.chunk); ok && e.state == stateStored && e.loc.Length == nd.loc.Length {
				nd.fromCache, nd.loc = true, e.loc
			}
		}
		s.mu.RLock()
		defer s.mu.RUnlock()
		return append([]digest.Digest(nil), s.packs...), nil
	}
	want := make(map[digest.Digest]int32, len(need))
	for _, i := range need {
		want[nodes[i].chunk] = i
	}
	var names []string
	if err := s.B.List(ctx, backend.Index, func(name string, _ int64) error {
		names = append(names, name)
		return nil
	}); err != nil {
		return nil, err
	}
	var packs []digest.Digest
	for _, name := range names {
		if len(want) == 0 {
			break
		}
		data, err := readAll(ctx, s.B, backend.Key{Kind: backend.Index, Name: name})
		if err != nil {
			return nil, err
		}
		if digest.Of(data).Hex() != name {
			return nil, fmt.Errorf("index %s: content does not match its name", name)
		}
		if bytes.HasPrefix(data, compMagic[:]) {
			continue // composites: a node cache reads stored chunks only
		}
		pack, entries, err := decodeIndex(data)
		if err != nil {
			return nil, fmt.Errorf("index %s: %v", name, err)
		}
		pid := -1
		for _, e := range entries {
			i, ok := want[e.Chunk]
			if !ok || e.Loc.Length != nodes[i].loc.Length {
				continue
			}
			if pid < 0 {
				pid = len(packs)
				packs = append(packs, pack)
			}
			loc := e.Loc
			loc.Pack = uint32(pid)
			nodes[i].fromCache, nodes[i].loc = true, loc
			delete(want, e.Chunk)
		}
	}
	return packs, nil
}
