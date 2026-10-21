package store

import (
	"context"
	"crypto/sha256"
	"hash"
	"os"
	"sync"

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
}

func (s *Store) newPackWriter(dir string) *packWriter {
	if dir == "" {
		dir = os.TempDir()
	}
	return &packWriter{s: s, dir: dir, seen: map[digest.Digest]bool{}}
}

// add packs one chunk unless the store or this writer already has it.
func (w *packWriter) add(ctx context.Context, d digest.Digest, stored []byte, id codec.ID, rawLen uint32) error {
	if w.s.has(d) {
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
