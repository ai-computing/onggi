package store

import (
	"bytes"
	"context"
	"slices"

	"github.com/ai-computing/onggi/pkg/digest"
)

// Diff (#38) measures what turning a node's copy of one artifact (the base) into another takes,
// at the level of stored chunks, without looking inside any file format (ADR-0009):
//
//   - Same: bytes whose chunk sits at the same offset of the paired base file. A copy of the base
//     file already has them, so applying the new artifact in place leaves them alone.
//   - Moved: bytes whose chunk the base holds somewhere else. The node has them but must copy them.
//   - New: bytes whose chunk the base lacks. Only these have to cross the network.
//
// The figures are lower bounds for byte-level sameness: bytes that chunk differently count as new.

// BlockSize is the file system block that reflink copies share (#38).
const BlockSize = 4096

// Extent is one stored chunk at Off in a file.
type Extent struct {
	Off   int64
	Len   uint32
	Chunk digest.Digest
}

// Extents returns, per file of m, the stored chunks that make it up, in file order.
func (s *Store) Extents(ctx context.Context, m *Manifest) ([][]Extent, error) {
	nodes, _, err := s.plan(ctx, m)
	if err != nil {
		return nil, err
	}
	return extentsOf(nodes, len(m.Files), true), nil
}

// extentsOf lists the stored chunks of each file, in file order if sorted is set.
func extentsOf(nodes []node, files int, sorted bool) [][]Extent {
	out := make([][]Extent, files)
	var place func(id int32, base int64, d digest.Digest, n uint32)
	place = func(id int32, base int64, d digest.Digest, n uint32) {
		nd := &nodes[id]
		if nd.hasT {
			out[nd.firstT.file] = append(out[nd.firstT.file], Extent{nd.firstT.off + base, n, d})
			for _, t := range nd.moreT {
				out[t.file] = append(out[t.file], Extent{t.off + base, n, d})
			}
		}
		if nd.hasP {
			place(nd.firstP.node, base+int64(nd.firstP.off), d, n)
			for _, e := range nd.moreP {
				place(e.node, base+int64(e.off), d, n)
			}
		}
	}
	for i := range nodes {
		if nodes[i].stored {
			place(int32(i), 0, nodes[i].chunk, nodes[i].loc.Length)
		}
	}
	for _, es := range out {
		if !sorted {
			break
		}
		slices.SortFunc(es, func(a, b Extent) int {
			switch {
			case a.Off < b.Off:
				return -1
			case a.Off > b.Off:
				return 1
			}
			return 0
		})
	}
	return out
}

// FileDiff is the diff of one file of the new artifact.
type FileDiff struct {
	Path        string `json:"path"`
	Base        string `json:"base,omitempty"` // the paired base file; empty: none
	Size        int64  `json:"size"`
	Same        int64  `json:"same"`
	Moved       int64  `json:"moved"`
	New         int64  `json:"new"`
	SameBlocks  int64  `json:"same_blocks"`  // bytes in whole BlockSize blocks that are all Same
	ChangedRuns int    `json:"changed_runs"` // maximal runs of bytes that are not Same
	LargestRun  int64  `json:"largest_run"`
	// Changed lists the changed runs as [offset, length] when DiffOptions.Ranges is set (#46):
	// every other byte is at the same offset in Base. A runtime that holds Base can update only
	// these ranges.
	Changed [][2]int64 `json:"changed,omitempty"`
	// Ranges is the range plan of the file when DiffOptions.Ranges is set (ADR-0010): runs that
	// cover the file in order, each the same, elsewhere in the base, or new.
	Ranges []Range `json:"ranges,omitempty"`
}

// RangePlanSchema names the format of DiffStats as JSON (ADR-0010). The range plan (Ranges)
// is the only interface between onggi and a runtime's device binding.
const RangePlanSchema = "onggi.rangeplan.v1"

// Range kinds (ADR-0010).
const (
	RangeSame      = "same"      // at the same offset in the paired base file
	RangeElsewhere = "elsewhere" // in the base at SourceFile, SourceOffset
	RangeNew       = "new"       // not in the base
)

// Range is one run of a file of the new artifact whose bytes are all of one kind.
type Range struct {
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
	Kind   string `json:"kind"`
	// for elsewhere: a base file and offset that hold the same bytes
	SourceFile   string `json:"source_file,omitempty"`
	SourceOffset *int64 `json:"source_offset,omitempty"`
}

// DiffOptions controls a diff.
type DiffOptions struct {
	Ranges bool // list the changed runs of each file
}

// DiffStats sums a diff over the files.
type DiffStats struct {
	Schema       string     `json:"schema"`        // RangePlanSchema
	BaseManifest string     `json:"base_manifest"` // what the plan applies to
	Manifest     string     `json:"manifest"`      // what it produces
	Files        []FileDiff `json:"files"`
	Size         int64      `json:"size"`
	Same         int64      `json:"same"`
	Moved        int64      `json:"moved"`
	New          int64      `json:"new"`
	NewStored    int64      `json:"new_stored"` // stored (compressed) bytes of the distinct new chunks
	SameBlocks   int64      `json:"same_blocks"`
	ChangedRuns  int        `json:"changed_runs"`
	LargestRun   int64      `json:"largest_run"`
}

// pairFiles pairs each file of m with a file of base: the same path, else an unpaired base file of
// the same kind, the closest in size, else of any kind. Variants of a model often name their files
// by their shape.
func pairFiles(base, m *Manifest) []int {
	pair := make([]int, len(m.Files))
	used := make([]bool, len(base.Files))
	byPath := map[string]int{}
	for j, f := range base.Files {
		byPath[f.Path] = j
	}
	for i, f := range m.Files {
		pair[i] = -1
		if j, ok := byPath[f.Path]; ok {
			pair[i], used[j] = j, true
		}
	}
	for _, sameKind := range []bool{true, false} {
		for i, f := range m.Files {
			if pair[i] >= 0 {
				continue
			}
			best := -1
			for j, g := range base.Files {
				if used[j] || (sameKind && g.Kind != f.Kind) {
					continue
				}
				if best < 0 || abs(g.Size-f.Size) < abs(base.Files[best].Size-f.Size) {
					best = j
				}
			}
			if best >= 0 {
				pair[i], used[best] = best, true
			}
		}
	}
	return pair
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// Diff compares artifact m against base, both in s.
func (s *Store) Diff(ctx context.Context, base, m digest.Digest, o DiffOptions) (DiffStats, error) {
	st := DiffStats{Schema: RangePlanSchema, BaseManifest: base.String(), Manifest: m.String()}
	bm, err := s.ReadManifest(ctx, base)
	if err != nil {
		return st, err
	}
	nm, err := s.ReadManifest(ctx, m)
	if err != nil {
		return st, err
	}
	be, err := s.Extents(ctx, bm)
	if err != nil {
		return st, err
	}
	ne, err := s.Extents(ctx, nm)
	if err != nil {
		return st, err
	}
	nodes, _, err := s.plan(ctx, nm)
	if err != nil {
		return st, err
	}
	// every chunk the base holds, sorted, for "somewhere in the base"
	var held []digest.Digest
	for _, es := range be {
		for _, e := range es {
			held = append(held, e.Chunk)
		}
	}
	cmp := func(a, b digest.Digest) int { return bytes.Compare(a[:], b[:]) }
	slices.SortFunc(held, cmp)
	held = slices.Compact(held)
	inBase := func(d digest.Digest) bool { _, ok := slices.BinarySearchFunc(held, d, cmp); return ok }
	type place struct {
		file int
		off  int64
	}
	var where map[digest.Digest]place // the first place in the base of each chunk, for the range plan
	if o.Ranges {
		where = make(map[digest.Digest]place, len(held))
		for j, es := range be {
			for _, e := range es {
				if _, ok := where[e.Chunk]; !ok {
					where[e.Chunk] = place{j, e.Off}
				}
			}
		}
	}

	pair := pairFiles(bm, nm)
	for i, f := range nm.Files {
		fd := FileDiff{Path: f.Path, Size: f.Size}
		var bes []Extent
		if pair[i] >= 0 {
			fd.Base = bm.Files[pair[i]].Path
			bes = be[pair[i]]
		}
		k := 0
		var sameFrom, sameTo int64 = -1, -1 // the current run of Same bytes
		var changedFrom int64 = -1
		closeSame := func() {
			if sameFrom < 0 {
				return
			}
			first := (sameFrom + BlockSize - 1) / BlockSize
			last := sameTo / BlockSize
			if sameTo == f.Size {
				last = (sameTo + BlockSize - 1) / BlockSize
			}
			if last > first {
				fd.SameBlocks += min(last*BlockSize, f.Size) - first*BlockSize
			}
			sameFrom = -1
		}
		addRange := func(e Extent, kind string) {
			if !o.Ranges {
				return
			}
			r := Range{Offset: e.Off, Length: int64(e.Len), Kind: kind}
			if kind == RangeElsewhere {
				p := where[e.Chunk]
				r.SourceFile, r.SourceOffset = bm.Files[p.file].Path, &p.off
			}
			if n := len(fd.Ranges); n > 0 {
				last := &fd.Ranges[n-1]
				if last.Kind == kind && last.Offset+last.Length == r.Offset && (kind != RangeElsewhere ||
					last.SourceFile == r.SourceFile && *last.SourceOffset+last.Length == *r.SourceOffset) {
					last.Length += r.Length
					return
				}
			}
			fd.Ranges = append(fd.Ranges, r)
		}
		closeChanged := func(end int64) {
			if changedFrom < 0 {
				return
			}
			fd.ChangedRuns++
			if o.Ranges {
				fd.Changed = append(fd.Changed, [2]int64{changedFrom, end - changedFrom})
			}
			fd.LargestRun = max(fd.LargestRun, end-changedFrom)
			changedFrom = -1
		}
		for _, e := range ne[i] {
			for k < len(bes) && bes[k].Off < e.Off {
				k++
			}
			end := e.Off + int64(e.Len)
			if k < len(bes) && bes[k].Off == e.Off && bes[k].Chunk == e.Chunk {
				addRange(e, RangeSame)
				fd.Same += int64(e.Len)
				closeChanged(e.Off)
				if sameFrom < 0 {
					sameFrom = e.Off
				}
				sameTo = end
				continue
			}
			closeSame()
			if changedFrom < 0 {
				changedFrom = e.Off
			}
			if inBase(e.Chunk) {
				addRange(e, RangeElsewhere)
				fd.Moved += int64(e.Len)
			} else {
				addRange(e, RangeNew)
				fd.New += int64(e.Len)
			}
		}
		closeSame()
		closeChanged(f.Size)
		st.Files = append(st.Files, fd)
		st.Size += fd.Size
		st.Same += fd.Same
		st.Moved += fd.Moved
		st.New += fd.New
		st.SameBlocks += fd.SameBlocks
		st.ChangedRuns += fd.ChangedRuns
		st.LargestRun = max(st.LargestRun, fd.LargestRun)
	}
	for i := range nodes {
		if nodes[i].stored && !inBase(nodes[i].chunk) {
			st.NewStored += nodes[i].loc.share()
		}
	}
	return st, nil
}
