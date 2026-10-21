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
	"github.com/ai-computing/onggi/pkg/layout"
)

// Store is an open store.
type Store struct {
	B      backend.Backend
	Config Config

	mu    sync.RWMutex
	index map[digest.Digest]Loc        // every chunk the store holds as bytes in a pack
	comps map[digest.Digest][]ChunkRef // chunks the store holds as sub-chunks
	packs []digest.Digest              // Loc.Pack numbers these
	packN map[digest.Digest]uint32

	loadMu sync.Mutex // LoadIndex runs once
	loaded bool
}

// DefaultPackSize keeps object counts low on any backend (ADR-0001).
const DefaultPackSize = 64 << 20

var configKey = backend.Key{Kind: backend.Config, Name: "store"}

// DefaultSub is the sub-chunk setting for new stores: 1 KiB / 4 KiB / 16 KiB. At 4 KiB, ATOM
// prefill variants share 78–87 % of their bytes instead of 29–37 % at 64 KiB (#15).
var DefaultSub = chunker.Params{Min: 1 << 10, Avg: 4 << 10, Max: 16 << 10}

// Create holds the settings a new store is created with. An existing store keeps its own.
type Create struct {
	Chunker  chunker.Params
	Sub      *chunker.Params // nil: one level of chunks
	SubKinds []layout.Kind   // file kinds that get sub-chunks; empty: all
	PackSize int64
}

// DefaultSubKinds are the kinds whose compile variants rearrange the same bytes (#15).
var DefaultSubKinds = []layout.Kind{layout.ATOMPrefill, layout.ATOMDecode, layout.ATOMOther}

// Open reads the store's config, creating it with p and packSize (one level of chunks) if the
// store is empty, and loads the chunk index.
func Open(ctx context.Context, b backend.Backend, p chunker.Params, packSize int64) (*Store, error) {
	return OpenWith(ctx, b, Create{Chunker: p, PackSize: packSize})
}

// OpenWith is Open with every creation setting.
func OpenWith(ctx context.Context, b backend.Backend, c Create) (*Store, error) {
	p, packSize := c.Chunker, c.PackSize
	cfg, err := readConfig(ctx, b)
	if errors.Is(err, backend.ErrNotFound) {
		if err := p.Validate(); err != nil {
			return nil, err
		}
		if c.Sub != nil {
			if err := c.Sub.Validate(); err != nil {
				return nil, fmt.Errorf("sub-chunks: %v", err)
			}
			if c.Sub.Max > p.Min {
				return nil, fmt.Errorf("sub-chunk max %d exceeds chunk min %d", c.Sub.Max, p.Min)
			}
		}
		if packSize <= 0 {
			packSize = DefaultPackSize
		}
		cfg = Config{Schema: "onggi.store.v1", Chunker: p, PackSize: packSize, Sub: c.Sub, Created: time.Now().UTC()}
		if c.Sub != nil {
			cfg.SubKinds = c.SubKinds
		}
		data, _ := json.MarshalIndent(cfg, "", "  ")
		if err := b.Put(ctx, configKey, bytes.NewReader(data), int64(len(data))); err != nil {
			return nil, err
		}
		cfg, err = readConfig(ctx, b) // a concurrent creator may have won; use what is stored
	}
	if err != nil {
		return nil, err
	}
	s := &Store{B: b, Config: cfg, index: map[digest.Digest]Loc{}, comps: map[digest.Digest][]ChunkRef{},
		packN: map[digest.Digest]uint32{}}
	return s, nil
}

// LoadIndex reads the store's index once. Push needs it; a pull with a locator does not, so
// Open leaves it unread.
func (s *Store) LoadIndex(ctx context.Context) error {
	s.loadMu.Lock()
	defer s.loadMu.Unlock()
	if s.loaded {
		return nil
	}
	if err := s.loadIndex(ctx); err != nil {
		return err
	}
	s.loaded = true
	return nil
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
	if cfg.Sub != nil {
		if err := cfg.Sub.Validate(); err != nil {
			return cfg, fmt.Errorf("store config: sub-chunks: %v", err)
		}
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

// loadIndex reads every pack index and composite table. A store of 45 GB has about 700 indexes.
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
	if bytes.HasPrefix(data, compMagic[:]) {
		cs, err := decodeComposites(data)
		if err != nil {
			return fmt.Errorf("composite table %s: %v", name, err)
		}
		s.addComposites(cs)
		return nil
	}
	pack, entries, err := decodeIndex(data)
	if err != nil {
		return fmt.Errorf("index %s: %v", name, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.packN[pack]
	if !ok {
		id = uint32(len(s.packs))
		s.packs = append(s.packs, pack)
		s.packN[pack] = id
	}
	for _, e := range entries {
		if _, ok := s.index[e.Chunk]; !ok {
			e.Loc.Pack = id
			s.index[e.Chunk] = e.Loc
		}
	}
	return nil
}

func (s *Store) packName(id uint32) digest.Digest {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.packs[id]
}

func (s *Store) addComposites(cs []composite) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range cs {
		if _, ok := s.comps[c.Chunk]; !ok {
			s.comps[c.Chunk] = c.Subs
		}
	}
}

// subChunks reports whether files of this path get sub-chunks.
func (s *Store) subChunks(rel string) bool {
	if s.Config.Sub == nil {
		return false
	}
	if len(s.Config.SubKinds) == 0 {
		return true
	}
	k := layout.Of(rel)
	for _, x := range s.Config.SubKinds {
		if x == k {
			return true
		}
	}
	return false
}

// has reports whether the store can produce chunk d, as bytes or as sub-chunks.
func (s *Store) has(d digest.Digest) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.index[d]; ok {
		return true
	}
	_, ok := s.comps[d]
	return ok
}

// Stats summarizes the loaded index: chunks held as bytes, packs, and chunks held as sub-chunks.
func (s *Store) Stats() (chunks, packs int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.index), len(s.packs)
}

// Composites is the number of chunks held as sub-chunks.
func (s *Store) Composites() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.comps)
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
