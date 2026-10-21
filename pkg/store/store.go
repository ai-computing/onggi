// Package store keeps artifacts in a backend as content-defined chunks packed into larger
// objects, with an index per pack, a chunk list per file and a JSON manifest per artifact.
//
// The commit order makes every crash safe: packs and their indexes first, then chunk lists, then
// the manifest, then the ref. A reader only follows a ref, and everything a ref reaches was
// written before it, so a crash leaves only unreferenced objects for GC.
package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/ai-computing/onggi/pkg/backend"
	"github.com/ai-computing/onggi/pkg/chunker"
	"github.com/ai-computing/onggi/pkg/digest"
)

// Store is an open store.
type Store struct {
	B      backend.Backend
	Config Config

	mu    sync.RWMutex
	index map[digest.Digest]Loc // every chunk the store holds
	packs int
}

// DefaultPackSize keeps object counts low on any backend (ADR-0001).
const DefaultPackSize = 64 << 20

var configKey = backend.Key{Kind: backend.Config, Name: "store"}

// Open reads the store's config, creating it with p and packSize if the store is empty, and
// loads the chunk index.
func Open(ctx context.Context, b backend.Backend, p chunker.Params, packSize int64) (*Store, error) {
	cfg, err := readConfig(ctx, b)
	if errors.Is(err, backend.ErrNotFound) {
		if err := p.Validate(); err != nil {
			return nil, err
		}
		if packSize <= 0 {
			packSize = DefaultPackSize
		}
		cfg = Config{Schema: "onggi.store.v1", Chunker: p, PackSize: packSize, Created: time.Now().UTC()}
		data, _ := json.MarshalIndent(cfg, "", "  ")
		if err := b.Put(ctx, configKey, bytes.NewReader(data), int64(len(data))); err != nil {
			return nil, err
		}
		cfg, err = readConfig(ctx, b) // a concurrent creator may have won; use what is stored
	}
	if err != nil {
		return nil, err
	}
	s := &Store{B: b, Config: cfg, index: map[digest.Digest]Loc{}}
	return s, s.loadIndex(ctx)
}

func readConfig(ctx context.Context, b backend.Backend) (Config, error) {
	var cfg Config
	data, err := readAll(ctx, b, configKey)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("store config: %v", err)
	}
	if cfg.Schema != "onggi.store.v1" {
		return cfg, fmt.Errorf("store config: unknown schema %q", cfg.Schema)
	}
	return cfg, cfg.Chunker.Validate()
}

func readAll(ctx context.Context, b backend.Backend, k backend.Key) ([]byte, error) {
	r, err := b.Get(ctx, k, 0, -1)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// loadIndex reads every pack index. A store of 45 GB has about 700 of them.
func (s *Store) loadIndex(ctx context.Context) error {
	var names []string
	if err := s.B.List(ctx, backend.Index, func(name string, _ int64) error {
		names = append(names, name)
		return nil
	}); err != nil {
		return err
	}
	for _, name := range names {
		data, err := readAll(ctx, s.B, backend.Key{Kind: backend.Index, Name: name})
		if err != nil {
			return err
		}
		if err := s.addIndex(name, data); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) addIndex(name string, data []byte) error {
	if digest.Of(data).Hex() != name {
		return fmt.Errorf("index %s: content does not match its name", name)
	}
	_, entries, err := decodeIndex(data)
	if err != nil {
		return fmt.Errorf("index %s: %v", name, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range entries {
		if _, ok := s.index[e.Chunk]; !ok {
			s.index[e.Chunk] = e.Loc
		}
	}
	s.packs++
	return nil
}

// Stats summarizes the loaded index.
func (s *Store) Stats() (chunks, packs int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.index), s.packs
}

// Resolve turns a ref name or a "sha256:" digest into a manifest digest.
func Resolve(ctx context.Context, refs backend.Refs, s string) (digest.Digest, error) {
	if d, err := digest.Parse(s); err == nil {
		return d, nil
	}
	if refs == nil {
		return digest.Digest{}, fmt.Errorf("%q is not a digest and the store has no refs", s)
	}
	v, err := refs.GetRef(ctx, s)
	if err != nil {
		return digest.Digest{}, err
	}
	return digest.Parse(v)
}

// ReadManifest loads and checks a manifest.
func (s *Store) ReadManifest(ctx context.Context, d digest.Digest) (*Manifest, error) {
	data, err := readAll(ctx, s.B, backend.Key{Kind: backend.Manifest, Name: d.Hex()})
	if err != nil {
		return nil, err
	}
	if digest.Of(data) != d {
		return nil, fmt.Errorf("manifest %s: content does not match its digest", d)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("manifest %s: %v", d, err)
	}
	if m.Schema != "onggi.manifest.v1" {
		return nil, fmt.Errorf("manifest %s: unknown schema %q", d, m.Schema)
	}
	return &m, nil
}

func (s *Store) readList(ctx context.Context, d digest.Digest) ([]ChunkRef, error) {
	data, err := readAll(ctx, s.B, backend.Key{Kind: backend.List, Name: d.Hex()})
	if err != nil {
		return nil, err
	}
	if digest.Of(data) != d {
		return nil, fmt.Errorf("chunk list %s: content does not match its digest", d)
	}
	return decodeList(data)
}
