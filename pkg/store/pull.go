package store

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ai-computing/onggi/pkg/backend"
	"github.com/ai-computing/onggi/pkg/codec"
	"github.com/ai-computing/onggi/pkg/digest"
)

// PullOptions controls a pull.
type PullOptions struct {
	Workers  int   // ranges read at once
	MaxRange int64 // largest single read; adjacent chunks of a pack are read together up to this
	// Cache is the node's chunk cache (#23): a local store whose chunks are read instead of the
	// store's, and which receives every chunk read from the store, so the next pull of a similar
	// artifact reads only what the node lacks. Its index is loaded by Pull.
	Cache    *Store
	CacheTmp string // where cache packs are assembled (default: system temp)
	// Peers are other nodes' caches (pkg/peer, #34), asked for what the node cache lacks before
	// the store.
	Peers []ChunkSource
	// Bases are artifacts on the node the pull may turn into this one (delta apply, #41). The pull
	// picks the one that keeps the most bytes in place, if that is at least MinSame (default
	// DefaultMinSame) of the artifact, and calls Claim, which hands the base's files over and
	// returns the directory they are in now (ok false: not available). The caller removes that
	// directory after the pull; the files the pull used are gone from it.
	Bases   []Base
	Claim   func(Base) (dir string, ok bool)
	MinSame float64
}

// PullStats reports a pull.
type PullStats struct {
	Files      int           `json:"files"`
	Bytes      int64         `json:"bytes"`       // logical bytes written
	ReadBytes  int64         `json:"read_bytes"`  // bytes read from the store (stored form)
	CacheBytes int64         `json:"cache_bytes"` // bytes read from the node cache
	PeerBytes  int64         `json:"peer_bytes"`  // bytes received from peers
	PeerChunks int64         `json:"peer_chunks"`
	Cached     int64         `json:"cached"` // stored bytes added to the node cache
	Base       string        `json:"base,omitempty"` // the artifact this pull turned into the new one
	Reused     int64         `json:"reused"`         // bytes left in place in the base's files
	Ranges     int           `json:"ranges"`
	Duration   time.Duration `json:"duration_ns"`
}

type target struct {
	file int32
	off  int64
}

// edge says that a chunk lies at off inside the composite chunk node.
type edge struct {
	node int32
	off  uint32
}

// node is one distinct chunk of a pull. A stored node has bytes in a pack; a composite node
// is its sub-chunks. Targets are the places a file list names the chunk; parents are the
// composites that contain it. A stored chunk's bytes go to its own targets and, through its
// parents, to theirs. Expanding composites into per-target sub-chunk writes instead would
// multiply the plan by the sub-chunks per chunk (#17). Most nodes have at most one target and
// one parent, so the first of each is inline.
type node struct {
	chunk     digest.Digest
	loc       Loc
	stored    bool
	fromCache bool // read from the node cache (loc is in the cache's packs)
	hasT      bool
	hasP      bool
	firstT    target
	moreT     []target
	firstP    edge
	moreP     []edge
}

// planner builds the nodes of one pull.
type planner struct {
	s     *Store
	idx   map[digest.Digest]int32
	nodes []node
}

// Pull writes the artifact into dest. Each distinct chunk is read once, its hash checked, and
// written to every place it occurs. Files appear under their final names only when complete.
func (s *Store) Pull(ctx context.Context, manifest digest.Digest, dest string, o PullOptions) (PullStats, error) {
	start := time.Now()
	if o.Workers < 1 {
		o.Workers = 8
	}
	if o.MaxRange <= 0 {
		o.MaxRange = 16 << 20
	}
	m, err := s.ReadManifest(ctx, manifest)
	if err != nil {
		return PullStats{}, err
	}
	var st PullStats

	// open every file under a temporary name, sized up front
	handles := make([]*os.File, len(m.Files))
	finals := make([]string, len(m.Files))
	defer func() {
		for _, h := range handles {
			if h != nil {
				h.Close()
				os.Remove(h.Name())
			}
		}
	}()
	for i, f := range m.Files {
		p, err := safeJoin(dest, f.Path)
		if err != nil {
			return st, err
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return st, err
		}
		h, err := os.Create(p + ".onggi-partial")
		if err != nil {
			return st, err
		}
		handles[i], finals[i] = h, p
		if err := h.Truncate(f.Size); err != nil {
			return st, err
		}
	}
	nodes, packs, err := s.plan(ctx, m)
	if err != nil {
		return st, err
	}
	done := make([]bool, len(nodes)) // in place from a base, or written from peers
	if len(o.Bases) > 0 && o.Claim != nil {
		minSame := o.MinSame
		if minSame <= 0 {
			minSame = DefaultMinSame
		}
		if p, ok := s.chooseBase(ctx, m, extentsOf(nodes, len(m.Files)), o.Bases, minSame); ok {
			if dir, ok := o.Claim(p.base); ok {
				st.Base = p.base.Manifest.String()
				if st.Reused, err = rebase(p, dir, m, nodes, handles, done); err != nil {
					return st, err
				}
			}
		}
	}
	srcs := []source{{s, packs}}
	var fill *packWriter
	if c := o.Cache; c != nil {
		// the lease keeps the cache's GC from evicting chunks this pull reads or adds (#31)
		release, err := c.Lease(ctx, "pull "+manifest.String())
		if err != nil {
			return st, fmt.Errorf("node cache: %w", err)
		}
		defer release()
		if err := c.LoadIndex(ctx); err != nil {
			return st, fmt.Errorf("node cache: %w", err)
		}
		for i := range nodes {
			nd := &nodes[i]
			if !nd.stored {
				continue
			}
			if _, e, ok := c.lookup(nd.chunk); ok && e.state == stateStored && e.loc.Length == nd.loc.Length {
				nd.fromCache, nd.loc = true, e.loc
			}
		}
		c.mu.RLock()
		srcs = append(srcs, source{c, append([]digest.Digest(nil), c.packs...)})
		c.mu.RUnlock()
		fill = c.newPackWriter(o.CacheTmp)
		defer fill.abort()
	}

	if len(o.Peers) > 0 {
		st.PeerBytes, st.PeerChunks = s.fromPeers(ctx, o.Peers, o.Workers, nodes, done, handles, fill)
	}

	// stored chunks by source and pack order, coalesced into ranges of that order
	var order []int32
	for i := range nodes {
		if nodes[i].stored && !done[i] {
			order = append(order, int32(i))
		}
	}
	sort.Slice(order, func(i, j int) bool {
		x, y := &nodes[order[i]], &nodes[order[j]]
		if x.fromCache != y.fromCache {
			return y.fromCache
		}
		a, b := x.loc, y.loc
		if a.Pack != b.Pack {
			return a.Pack < b.Pack
		}
		return a.Offset < b.Offset
	})
	type span struct{ a, b int } // order[a:b]
	var ranges []span
	for k, i := range order {
		l := nodes[i].loc
		if n := len(ranges); n > 0 {
			r := &ranges[n-1]
			first, prev := nodes[order[r.a]].loc, nodes[order[r.b-1]].loc
			if nodes[order[r.b-1]].fromCache == nodes[i].fromCache && prev.Pack == l.Pack && prev.Offset+prev.Stored == l.Offset &&
				int64(l.Offset+l.Stored-first.Offset) <= o.MaxRange {
				r.b = k + 1
				continue
			}
		}
		ranges = append(ranges, span{k, k + 1})
	}
	st.Ranges = len(ranges)

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	jobs := make(chan span)
	var wg sync.WaitGroup
	var readBytes, cacheBytes atomic.Int64
	for w := 0; w < o.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var buf []byte
			for r := range jobs {
				if ctx.Err() != nil {
					continue
				}
				ids := order[r.a:r.b]
				src, fl := srcs[0], fill
				if nodes[ids[0]].fromCache {
					src, fl = srcs[1], nil
				}
				n, err := src.store.readRange(ctx, nodes, src.packs, ids, handles, &buf, fl)
				if err != nil {
					cancel(err)
					continue
				}
				if fl == nil && o.Cache != nil {
					cacheBytes.Add(n)
				} else {
					readBytes.Add(n)
				}
			}
		}()
	}
	for _, r := range ranges {
		jobs <- r
	}
	close(jobs)
	wg.Wait()
	if err := context.Cause(ctx); err != nil {
		return st, err
	}

	for i, f := range m.Files {
		h := handles[i]
		if err := h.Sync(); err != nil {
			return st, err
		}
		if err := h.Close(); err != nil {
			return st, err
		}
		handles[i] = nil
		if err := os.Chmod(h.Name(), os.FileMode(f.Mode)&0o777); err != nil {
			return st, err
		}
		if err := os.Rename(h.Name(), finals[i]); err != nil {
			return st, err
		}
		st.Files++
		st.Bytes += f.Size
	}
	if fill != nil {
		if err := fill.close(ctx); err != nil {
			return st, fmt.Errorf("node cache: %w", err)
		}
		st.Cached = fill.bytes
		if err := o.Cache.recordUse(ctx, manifest, time.Now()); err != nil {
			return st, fmt.Errorf("node cache: %w", err)
		}
	}
	st.ReadBytes, st.CacheBytes = readBytes.Load(), cacheBytes.Load()
	st.Duration = time.Since(start)
	return st, nil
}

func (s *Store) anyMissing(ctx context.Context, packs []digest.Digest) (bool, error) {
	keys := make([]backend.Key, len(packs))
	for i, p := range packs {
		keys[i] = backend.Key{Kind: backend.Pack, Name: p.Hex()}
	}
	have, err := s.B.Has(ctx, keys)
	if err != nil {
		return false, err
	}
	for _, h := range have {
		if !h {
			return true, nil
		}
	}
	return false, nil
}

// plan returns the chunk graph of m and the packs its Locs index.
func (s *Store) plan(ctx context.Context, m *Manifest) ([]node, []digest.Digest, error) {
	if m.Locator == nil {
		return s.planFromIndex(ctx, m)
	}
	nodes, packs, err := s.planFromLocator(ctx, m, *m.Locator)
	if err != nil {
		return nil, nil, err
	}
	// GC repacks live chunks into new packs and then removes the old ones; a locator written
	// before that names packs that are gone, and the index knows where the chunks are now
	if gone, err := s.anyMissing(ctx, packs); err != nil {
		return nil, nil, err
	} else if gone {
		return s.planFromIndex(ctx, m)
	}
	return nodes, packs, nil
}

// planFromIndex plans with the store's index and the chunk lists (manifests without a locator).
func (s *Store) planFromIndex(ctx context.Context, m *Manifest) ([]node, []digest.Digest, error) {
	if err := s.LoadIndex(ctx); err != nil {
		return nil, nil, err
	}
	pl := &planner{s: s, idx: map[digest.Digest]int32{}}
	for i, f := range m.Files {
		list, err := s.readList(ctx, f.List)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", f.Path, err)
		}
		var off int64
		for _, c := range list {
			id, err := pl.ref(c)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %w", f.Path, err)
			}
			pl.addTarget(id, target{file: int32(i), off: off})
			off += int64(c.Length)
		}
		if off != f.Size {
			return nil, nil, fmt.Errorf("%s: chunks add up to %d bytes, manifest says %d", f.Path, off, f.Size)
		}
	}
	s.mu.RLock()
	packs := append([]digest.Digest(nil), s.packs...)
	s.mu.RUnlock()
	return pl.nodes, packs, nil
}

// planFromLocator builds the same graph from the artifact's locator: no store index, no chunk
// lists, no map by digest.
func (s *Store) planFromLocator(ctx context.Context, m *Manifest, ld digest.Digest) ([]node, []digest.Digest, error) {
	data, err := readAll(ctx, s.B, backend.Key{Kind: backend.List, Name: ld.Hex()})
	if err != nil {
		return nil, nil, fmt.Errorf("locator %s: %w", ld, err)
	}
	if digest.Of(data) != ld {
		return nil, nil, fmt.Errorf("locator %s: content does not match its digest", ld)
	}
	nodes, packs, err := readLocator(data, m)
	if err != nil {
		return nil, nil, fmt.Errorf("locator %s: %w", ld, err)
	}
	return nodes, packs, nil
}

// ref returns the node of chunk c, creating it, and for a composite its sub-chunk nodes, on
// first use.
func (pl *planner) ref(c ChunkRef) (int32, error) {
	if id, ok := pl.idx[c.Digest]; ok {
		if pl.nodes[id].stored && pl.nodes[id].loc.Length != c.Length {
			return 0, fmt.Errorf("chunk %s length %d, %d in the index", c.Digest, c.Length, pl.nodes[id].loc.Length)
		}
		return id, nil
	}
	s := pl.s
	_, e, ok := s.lookup(c.Digest)
	loc := e.loc
	var subs []ChunkRef
	isComp := ok && e.state == stateComp
	if isComp {
		subs = s.subRefs(e)
	}
	id := int32(len(pl.nodes))
	switch {
	case ok && e.state == stateStored:
		if loc.Length != c.Length {
			return 0, fmt.Errorf("chunk %s length %d in the list, %d in the index", c.Digest, c.Length, loc.Length)
		}
		pl.idx[c.Digest] = id
		pl.nodes = append(pl.nodes, node{chunk: c.Digest, loc: loc, stored: true})
		return id, nil
	case isComp:
		pl.idx[c.Digest] = id
		pl.nodes = append(pl.nodes, node{chunk: c.Digest})
		var n int64
		// A sub-chunk may itself be a composite: another file stored those bytes as a whole chunk.
		// Each level is strictly shorter, so the recursion ends.
		for _, sc := range subs {
			if sc.Length >= c.Length {
				return 0, fmt.Errorf("chunk %s: sub-chunk %s is not shorter than the chunk", c.Digest, sc.Digest)
			}
			child, err := pl.ref(sc)
			if err != nil {
				return 0, fmt.Errorf("chunk %s: %w", c.Digest, err)
			}
			pl.addParent(child, edge{node: id, off: uint32(n)})
			n += int64(sc.Length)
		}
		if n != int64(c.Length) {
			return 0, fmt.Errorf("chunk %s: sub-chunks add up to %d bytes, want %d", c.Digest, n, c.Length)
		}
		return id, nil
	}
	return 0, fmt.Errorf("chunk %s is not in the store", c.Digest)
}

func (pl *planner) addTarget(id int32, t target) {
	nd := &pl.nodes[id]
	if !nd.hasT {
		nd.firstT, nd.hasT = t, true
		return
	}
	nd.moreT = append(nd.moreT, t)
}

func (pl *planner) addParent(id int32, e edge) {
	nd := &pl.nodes[id]
	if !nd.hasP {
		nd.firstP, nd.hasP = e, true
		return
	}
	nd.moreP = append(nd.moreP, e)
}

// emit writes raw, which sits at base inside node id, to every place node id goes.
func emit(nodes []node, id int32, raw []byte, base int64, handles []*os.File) error {
	nd := &nodes[id]
	if nd.hasT {
		if _, err := handles[nd.firstT.file].WriteAt(raw, nd.firstT.off+base); err != nil {
			return err
		}
		for _, t := range nd.moreT {
			if _, err := handles[t.file].WriteAt(raw, t.off+base); err != nil {
				return err
			}
		}
	}
	if nd.hasP {
		if err := emit(nodes, nd.firstP.node, raw, base+int64(nd.firstP.off), handles); err != nil {
			return err
		}
		for _, e := range nd.moreP {
			if err := emit(nodes, e.node, raw, base+int64(e.off), handles); err != nil {
				return err
			}
		}
	}
	return nil
}

// source is a store a pull reads from, with the pack names its Loc.Pack numbers index.
type source struct {
	store *Store
	packs []digest.Digest
}

// readRange reads one range of a pack into *buf (reused across calls of one worker), decodes and
// checks each chunk, writes it everywhere it goes, and hands it in stored form to fill (the
// node cache) when fill is set.
func (s *Store) readRange(ctx context.Context, nodes []node, packs []digest.Digest, ids []int32, handles []*os.File, buf *[]byte, fill *packWriter) (int64, error) {
	first, last := nodes[ids[0]].loc, nodes[ids[len(ids)-1]].loc
	pack := packs[first.Pack]
	n := int64(last.Offset+last.Stored) - int64(first.Offset)
	rc, err := s.B.Get(ctx, backend.Key{Kind: backend.Pack, Name: pack.Hex()}, int64(first.Offset), n)
	if err != nil {
		return 0, err
	}
	if int64(cap(*buf)) < n {
		*buf = make([]byte, n)
	}
	b := (*buf)[:n]
	_, err = io.ReadFull(rc, b)
	rc.Close()
	if err != nil {
		return 0, fmt.Errorf("pack %s: %w", pack, err)
	}
	for _, i := range ids {
		nd := &nodes[i]
		raw, err := codec.Decode(b[int64(nd.loc.Offset)-int64(first.Offset):][:nd.loc.Stored], nd.loc.Codec, int(nd.loc.Length))
		if err != nil {
			return 0, fmt.Errorf("pack %s offset %d: chunk %s: %w", pack, nd.loc.Offset, nd.chunk, err)
		}
		if digest.Of(raw) != nd.chunk {
			return 0, fmt.Errorf("pack %s offset %d: chunk %s fails its hash check", pack, nd.loc.Offset, nd.chunk)
		}
		if err := emit(nodes, i, raw, 0, handles); err != nil {
			return 0, err
		}
		if fill != nil {
			stored := b[int64(nd.loc.Offset)-int64(first.Offset):][:nd.loc.Stored]
			if err := fill.add(ctx, nd.chunk, stored, nd.loc.Codec, nd.loc.Length); err != nil {
				return 0, fmt.Errorf("node cache: %w", err)
			}
		}
	}
	return n, nil
}

// safeJoin keeps manifest paths inside dest.
func safeJoin(dest, rel string) (string, error) {
	clean := path.Clean("/" + rel)
	if clean == "/" || strings.Contains(rel, `\`) || clean != "/"+rel {
		return "", fmt.Errorf("unsafe path %q in manifest", rel)
	}
	return filepath.Join(dest, filepath.FromSlash(clean[1:])), nil
}
