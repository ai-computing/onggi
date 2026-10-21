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
}

// PullStats reports a pull.
type PullStats struct {
	Files     int           `json:"files"`
	Bytes     int64         `json:"bytes"`      // logical bytes written
	ReadBytes int64         `json:"read_bytes"` // bytes read from the backend (stored form)
	Ranges    int           `json:"ranges"`
	Duration  time.Duration `json:"duration_ns"`
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
	chunk  digest.Digest
	loc    Loc
	stored bool
	hasT   bool
	hasP   bool
	firstT target
	moreT  []target
	firstP edge
	moreP  []edge
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
	var nodes []node
	var packs []digest.Digest
	if m.Locator != nil {
		nodes, packs, err = s.planFromLocator(ctx, m, *m.Locator)
	} else {
		nodes, packs, err = s.planFromIndex(ctx, m)
	}
	if err != nil {
		return st, err
	}

	// stored chunks in pack order, coalesced into ranges of that order
	var order []int32
	for i := range nodes {
		if nodes[i].stored {
			order = append(order, int32(i))
		}
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := nodes[order[i]].loc, nodes[order[j]].loc
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
			if prev.Pack == l.Pack && prev.Offset+prev.Stored == l.Offset &&
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
	var readBytes atomic.Int64
	for w := 0; w < o.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var buf []byte
			for r := range jobs {
				if ctx.Err() != nil {
					continue
				}
				n, err := s.readRange(ctx, nodes, packs, order[r.a:r.b], handles, &buf)
				if err != nil {
					cancel(err)
					continue
				}
				readBytes.Add(n)
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
	st.ReadBytes = readBytes.Load()
	st.Duration = time.Since(start)
	return st, nil
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
	l, err := decodeLocator(data)
	data = nil
	if err != nil {
		return nil, nil, fmt.Errorf("locator %s: %v", ld, err)
	}
	if len(l.Files) != len(m.Files) {
		return nil, nil, fmt.Errorf("locator %s: %d files, manifest has %d", ld, len(l.Files), len(m.Files))
	}
	nodes := make([]node, len(l.Nodes))
	pl := &planner{nodes: nodes}
	for i := range l.Nodes {
		ln := &l.Nodes[i]
		nodes[i].chunk = ln.Chunk
		if !ln.Comp {
			nodes[i].stored, nodes[i].loc = true, ln.Loc
			continue
		}
		var n int64
		for _, k := range ln.Subs {
			if int(k) >= len(l.Nodes) || l.Nodes[k].Length >= ln.Length {
				return nil, nil, fmt.Errorf("locator %s: chunk %s has a bad sub-chunk", ld, ln.Chunk)
			}
			pl.addParent(int32(k), edge{node: int32(i), off: uint32(n)})
			n += int64(l.Nodes[k].Length)
		}
		if n != int64(ln.Length) {
			return nil, nil, fmt.Errorf("locator %s: chunk %s: sub-chunks add up to %d bytes, want %d", ld, ln.Chunk, n, ln.Length)
		}
		ln.Subs = nil
	}
	for i, f := range m.Files {
		var off int64
		for _, id := range l.Files[i] {
			if int(id) >= len(l.Nodes) {
				return nil, nil, fmt.Errorf("locator %s: %s names node %d of %d", ld, f.Path, id, len(l.Nodes))
			}
			pl.addTarget(int32(id), target{file: int32(i), off: off})
			off += int64(l.Nodes[id].Length)
		}
		if off != f.Size {
			return nil, nil, fmt.Errorf("%s: chunks add up to %d bytes, manifest says %d", f.Path, off, f.Size)
		}
	}
	return pl.nodes, l.Packs, nil
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
	s.mu.RLock()
	loc, ok := s.index[c.Digest]
	subs, isComp := s.comps[c.Digest]
	s.mu.RUnlock()
	id := int32(len(pl.nodes))
	switch {
	case ok:
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

// readRange reads one range of a pack into *buf (reused across calls of one worker), decodes and
// checks each chunk, and writes it everywhere it goes.
func (s *Store) readRange(ctx context.Context, nodes []node, packs []digest.Digest, ids []int32, handles []*os.File, buf *[]byte) (int64, error) {
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
