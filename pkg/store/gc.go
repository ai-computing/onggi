package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ai-computing/onggi/pkg/backend"
	"github.com/ai-computing/onggi/pkg/digest"
)

// GC is mark-and-sweep with a grace period (ADR-0005, #28).
//
// Roots are the refs and any manifests the caller keeps. Everything reachable from them is live:
// manifests, their locators and chunk lists, the chunks those name (through composites down to
// stored sub-chunks), the packs and pack indexes holding them, and the composite tables that
// describe live composites. Everything else is a candidate.
//
// Deletion is two-phase, so no object is deleted before it has been unreferenced for a grace
// period. A run records the candidates in a state object. A run at least Grace later, with no
// live lease, deletes the objects that were candidates then and still are now, in reverse
// commit order (manifests, lists and locators, composite tables and pack indexes, packs).
// Pushes hold a lease while they run, so a push that deduplicates against an old chunk is never
// raced by a sweep.
//
// A composite table holding live and dead composites is rewritten with its live ones first; the
// old table becomes a candidate once every live composite it holds is in a fully live table.
// Packs with any live chunk are kept; their dead bytes are reported for a later repack.

var gcStateKey = backend.Key{Kind: backend.Config, Name: "gc-state"}

const leasePrefix = "lease-"

// DefaultLeaseTTL bounds how long a lease counts: a push that crashed leaves its lease behind.
const DefaultLeaseTTL = 24 * time.Hour

type lease struct {
	Created time.Time `json:"created"`
	Purpose string    `json:"purpose"`
}

// Lease writes a lease object and returns the function that removes it. While a lease younger
// than the GC's lease TTL exists, GC deletes nothing.
func (s *Store) Lease(ctx context.Context, purpose string) (func(), error) {
	var b [8]byte
	rand.Read(b[:])
	k := backend.Key{Kind: backend.Config, Name: leasePrefix + hex.EncodeToString(b[:])}
	data, _ := json.Marshal(lease{Created: time.Now().UTC(), Purpose: purpose})
	if err := s.B.Put(ctx, k, bytes.NewReader(data), int64(len(data))); err != nil {
		return nil, fmt.Errorf("lease: %w", err)
	}
	return func() { s.B.Delete(context.WithoutCancel(ctx), k) }, nil
}

// GCOptions controls a GC run.
type GCOptions struct {
	Keep     []digest.Digest // manifests kept in addition to the refs
	Grace    time.Duration   // time between the run that records a candidate and the run that deletes it
	LeaseTTL time.Duration   // leases older than this are ignored (default DefaultLeaseTTL)
	DryRun   bool            // report only: no state, no rewrites, no deletions
	Now      func() time.Time
}

// GCStats reports a GC run.
type GCStats struct {
	Phase          string `json:"phase"` // marked, waiting, blocked, swept, dry-run
	Roots          int    `json:"roots"`
	LiveManifests  int    `json:"live_manifests"`
	LivePacks      int    `json:"live_packs"`
	Packs          int    `json:"packs"`
	Candidates     int    `json:"candidates"`
	CandidateBytes int64  `json:"candidate_bytes"`
	Deleted        int    `json:"deleted"`
	DeletedBytes   int64  `json:"deleted_bytes"`
	Rewritten      int    `json:"rewritten_tables"`
	DeadInLive     int64  `json:"dead_bytes_in_live_packs"`
	Leases         int    `json:"live_leases"`
	MarkedAt       string `json:"marked_at,omitempty"`
}

type gcState struct {
	Marked     time.Time `json:"marked"`
	Candidates []string  `json:"candidates"` // "kind/name"
}

type object struct {
	key  backend.Key
	size int64
}

// GC runs one round of mark and (when due) sweep.
func (s *Store) GC(ctx context.Context, refs backend.Refs, o GCOptions) (GCStats, error) {
	var st GCStats
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.LeaseTTL <= 0 {
		o.LeaseTTL = DefaultLeaseTTL
	}
	now := o.Now().UTC()
	if err := s.LoadIndex(ctx); err != nil {
		return st, err
	}

	// roots
	roots := append([]digest.Digest(nil), o.Keep...)
	if refs != nil {
		if err := refs.ListRefs(ctx, func(name, value string) error {
			d, err := digest.Parse(value)
			if err != nil {
				return fmt.Errorf("ref %s: %v", name, err)
			}
			roots = append(roots, d)
			return nil
		}); err != nil {
			return st, err
		}
	}
	st.Roots = len(roots)

	// mark
	s.mu.RLock()
	n := len(s.idx.ent)
	s.mu.RUnlock()
	live := make([]bool, n) // index entries reached
	livePack := map[uint32]int64{}
	liveObj := map[string]bool{}
	var markChunk func(c ChunkRef) error
	markChunk = func(c ChunkRef) error {
		id, e, ok := s.lookup(c.Digest)
		if !ok {
			return fmt.Errorf("chunk %s of a live artifact is not in the store", c.Digest)
		}
		if live[id] {
			return nil
		}
		live[id] = true
		if e.state == stateStored {
			livePack[e.loc.Pack] += int64(e.loc.Stored)
			return nil
		}
		for _, sc := range s.subRefs(e) {
			if err := markChunk(sc); err != nil {
				return err
			}
		}
		return nil
	}
	for _, md := range roots {
		if liveObj[keyString(backend.Manifest, md.Hex())] {
			continue
		}
		m, err := s.ReadManifest(ctx, md)
		if err != nil {
			return st, fmt.Errorf("root manifest %s: %w (nothing deleted)", md, err)
		}
		liveObj[keyString(backend.Manifest, md.Hex())] = true
		st.LiveManifests++
		if m.Locator != nil {
			liveObj[keyString(backend.List, m.Locator.Hex())] = true
		}
		for _, f := range m.Files {
			liveObj[keyString(backend.List, f.List.Hex())] = true
			list, err := s.readList(ctx, f.List)
			if err != nil {
				return st, fmt.Errorf("%s: %w (nothing deleted)", f.Path, err)
			}
			for _, c := range list {
				if err := markChunk(c); err != nil {
					return st, fmt.Errorf("%s: %w (nothing deleted)", f.Path, err)
				}
			}
		}
	}
	st.LivePacks = len(livePack)

	// classify every object
	var cands []object
	add := func(k backend.Key, size int64) {
		cands = append(cands, object{k, size})
		st.CandidateBytes += size
	}
	for _, kind := range []backend.Kind{backend.Manifest, backend.List} {
		if err := s.B.List(ctx, kind, func(name string, size int64) error {
			if !liveObj[keyString(kind, name)] {
				add(backend.Key{Kind: kind, Name: name}, size)
			}
			return nil
		}); err != nil {
			return st, err
		}
	}
	livePackName := map[string]bool{}
	s.mu.RLock()
	for id := range livePack {
		livePackName[s.packs[id].Hex()] = true
	}
	s.mu.RUnlock()
	packSize := map[string]int64{}
	if err := s.B.List(ctx, backend.Pack, func(name string, size int64) error {
		st.Packs++
		packSize[name] = size
		if !livePackName[name] {
			add(backend.Key{Kind: backend.Pack, Name: name}, size)
		}
		return nil
	}); err != nil {
		return st, err
	}
	for name, size := range packSize {
		if livePackName[name] {
			if d, err := digest.Parse("sha256:" + name); err == nil {
				s.mu.RLock()
				id, ok := s.packN[d]
				s.mu.RUnlock()
				if ok {
					st.DeadInLive += size - livePack[id]
				}
			}
		}
	}

	// index objects: pack indexes follow their pack; composite tables follow their composites
	var rewrite [][]composite // live parts of mixed tables
	type table struct {
		name  string
		size  int64
		live  []composite
		dead  int
		total int
	}
	var tables []table
	cleanHolds := map[digest.Digest]bool{} // live composites held by a fully live table
	var names []object
	if err := s.B.List(ctx, backend.Index, func(name string, size int64) error {
		names = append(names, object{backend.Key{Kind: backend.Index, Name: name}, size})
		return nil
	}); err != nil {
		return st, err
	}
	for _, ob := range names {
		data, err := readAll(ctx, s.B, ob.key)
		if err != nil {
			return st, err
		}
		if bytes.HasPrefix(data, compMagic[:]) {
			cs, err := decodeComposites(data)
			if err != nil {
				return st, fmt.Errorf("composite table %s: %v", ob.key.Name, err)
			}
			t := table{name: ob.key.Name, size: ob.size, total: len(cs)}
			for _, c := range cs {
				if id, _, ok := s.lookup(c.Chunk); ok && live[id] {
					t.live = append(t.live, c)
				} else {
					t.dead++
				}
			}
			if t.dead == 0 {
				for _, c := range t.live {
					cleanHolds[c.Chunk] = true
				}
			}
			tables = append(tables, t)
			continue
		}
		pack, _, err := decodeIndex(data)
		if err != nil {
			return st, fmt.Errorf("index %s: %v", ob.key.Name, err)
		}
		if !livePackName[pack.Hex()] {
			add(ob.key, ob.size)
		}
	}
	for _, t := range tables {
		switch {
		case t.dead == 0:
			// fully live
		case len(t.live) == 0:
			add(backend.Key{Kind: backend.Index, Name: t.name}, t.size)
		default:
			held := true
			for _, c := range t.live {
				if !cleanHolds[c.Chunk] {
					held = false
					break
				}
			}
			if held {
				add(backend.Key{Kind: backend.Index, Name: t.name}, t.size)
			} else {
				rewrite = append(rewrite, t.live)
			}
		}
	}
	st.Candidates = len(cands)

	if o.DryRun {
		st.Phase = "dry-run"
		return st, nil
	}
	// rewriting is always safe: it only adds a table of live composites
	for _, live := range rewrite {
		if err := s.putComposites(ctx, live); err != nil {
			return st, err
		}
		st.Rewritten++
	}

	prev, err := s.readGCState(ctx)
	if err != nil && !errors.Is(err, backend.ErrNotFound) {
		return st, err
	}
	writeState := func(c []object) error {
		gs := gcState{Marked: now}
		for _, ob := range c {
			gs.Candidates = append(gs.Candidates, keyString(ob.key.Kind, ob.key.Name))
		}
		sort.Strings(gs.Candidates)
		data, _ := json.MarshalIndent(gs, "", " ")
		// the state is replaced, not content-addressed: delete, then put
		if err := s.B.Delete(ctx, gcStateKey); err != nil {
			return err
		}
		return s.B.Put(ctx, gcStateKey, bytes.NewReader(data), int64(len(data)))
	}
	if errors.Is(err, backend.ErrNotFound) {
		st.Phase = "marked"
		st.MarkedAt = now.Format(time.RFC3339)
		return st, writeState(cands)
	}
	st.MarkedAt = prev.Marked.Format(time.RFC3339)
	if now.Sub(prev.Marked) < o.Grace {
		st.Phase = "waiting"
		return st, nil
	}
	leases, err := s.liveLeases(ctx, now, o.LeaseTTL)
	if err != nil {
		return st, err
	}
	st.Leases = leases
	if leases > 0 {
		st.Phase = "blocked"
		return st, nil
	}

	// sweep: candidates then and now, in reverse commit order
	was := map[string]bool{}
	for _, k := range prev.Candidates {
		was[k] = true
	}
	order := map[backend.Kind]int{backend.Manifest: 0, backend.List: 1, backend.Index: 2, backend.Pack: 3}
	sort.SliceStable(cands, func(i, j int) bool { return order[cands[i].key.Kind] < order[cands[j].key.Kind] })
	var keep []object
	for _, ob := range cands {
		if !was[keyString(ob.key.Kind, ob.key.Name)] {
			keep = append(keep, ob) // new candidate: its own grace starts now
			continue
		}
		if err := s.B.Delete(ctx, ob.key); err != nil {
			return st, err
		}
		st.Deleted++
		st.DeletedBytes += ob.size
	}
	st.Phase = "swept"
	return st, writeState(keep)
}

func keyString(k backend.Kind, name string) string { return string(k) + "/" + name }

func (s *Store) readGCState(ctx context.Context) (gcState, error) {
	var gs gcState
	data, err := readAll(ctx, s.B, gcStateKey)
	if err != nil {
		return gs, err
	}
	return gs, json.Unmarshal(data, &gs)
}

// liveLeases counts lease objects younger than ttl.
func (s *Store) liveLeases(ctx context.Context, now time.Time, ttl time.Duration) (int, error) {
	var keys []string
	if err := s.B.List(ctx, backend.Config, func(name string, _ int64) error {
		if strings.HasPrefix(name, leasePrefix) {
			keys = append(keys, name)
		}
		return nil
	}); err != nil {
		return 0, err
	}
	n := 0
	for _, name := range keys {
		data, err := readAll(ctx, s.B, backend.Key{Kind: backend.Config, Name: name})
		if errors.Is(err, backend.ErrNotFound) {
			continue // released meanwhile
		}
		if err != nil {
			return 0, err
		}
		var l lease
		if json.Unmarshal(data, &l) != nil || now.Sub(l.Created) < ttl {
			n++ // an unreadable lease counts as live: GC pauses when lease state is unknown
		}
	}
	return n, nil
}
