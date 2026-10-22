package store

import (
	"bytes"
	"cmp"
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/ai-computing/onggi/pkg/digest"
)

// Delta apply (#41): a pull may take over an artifact already on the node (a base) and turn it
// into the new one. The base's files move into place; every chunk that sits at the same offset
// in the paired base file is left as it is, and only the rest is fetched and written. Between
// two decode variants that is 1.4 % of 15 GB (#38). Nothing needs reflinks: the base is consumed,
// not copied. The base's files are trusted to match its manifest, as they did when it was pulled.

// Base is an artifact materialized on the node: its manifest and the directory holding its files.
type Base struct {
	Manifest digest.Digest
	Dir      string
}

// DefaultMinSame is the smallest share of bytes at the same offset for which a pull takes a base.
const DefaultMinSame = 0.5

type rebasePlan struct {
	base  Base
	files []File     // the base's files
	ext   [][]Extent // the base's extents, per base file
	pair  []int      // base file per file of the new artifact, -1: none
	same  int64
	moved int64     // bytes the base holds, but at another offset (#58)
	held  []baseRef // the base's extents by chunk digest
}

// baseRef is one extent of a base: ext[file][i].
type baseRef struct{ file, i int32 }

// byDigest indexes a base's extents by chunk digest.
func byDigest(ext [][]Extent) []baseRef {
	var refs []baseRef
	for f, es := range ext {
		for i := range es {
			refs = append(refs, baseRef{int32(f), int32(i)})
		}
	}
	slices.SortFunc(refs, func(a, b baseRef) int {
		return bytes.Compare(ext[a.file][a.i].Chunk[:], ext[b.file][b.i].Chunk[:])
	})
	return refs
}

// find returns where the base holds chunk d: the base file and the extent.
func (p *rebasePlan) find(d digest.Digest) (int, Extent, bool) {
	k, ok := slices.BinarySearchFunc(p.held, d, func(r baseRef, d digest.Digest) int {
		return bytes.Compare(p.ext[r.file][r.i].Chunk[:], d[:])
	})
	if !ok {
		return 0, Extent{}, false
	}
	r := p.held[k]
	return int(r.file), p.ext[r.file][r.i], true
}

// chooseBase returns the base that keeps the most bytes in place, and whether that is at least
// minSame of m's bytes; and the base that holds the most of m's bytes anywhere, and whether that
// is at least minSame (#58).
func (s *Store) chooseBase(ctx context.Context, m *Manifest, newExt [][]Extent, bases []Base, minSame float64) (rebasePlan, bool, rebasePlan, bool) {
	var best, most rebasePlan
	for _, b := range bases {
		bm, err := s.ReadManifest(ctx, b.Manifest)
		if err != nil {
			continue // the store no longer has it: not usable
		}
		bnodes, _, err := s.plan(ctx, bm)
		if err != nil {
			continue
		}
		p := rebasePlan{base: b, files: bm.Files, ext: extentsOf(bnodes, len(bm.Files), true), pair: pairFiles(bm, m)}
		p.held = byDigest(p.ext)
		for i, j := range p.pair {
			if j >= 0 && !filepath.IsLocal(filepath.FromSlash(bm.Files[j].Path)) {
				p.pair[i] = -1
			}
		}
		for i := range m.Files {
			j := p.pair[i]
			for _, e := range newExt[i] {
				if j >= 0 && sameAt(p.ext[j], e) {
					p.same += int64(e.Len)
				} else if f, _, ok := p.find(e.Chunk); ok && filepath.IsLocal(filepath.FromSlash(bm.Files[f].Path)) {
					p.moved += int64(e.Len)
				}
			}
		}
		if p.same > best.same {
			best = p
		}
		if p.same+p.moved > most.same+most.moved {
			most = p
		}
	}
	need := minSame * float64(m.Bytes)
	return best, best.same > 0 && float64(best.same) >= need, most, most.same+most.moved > 0 && float64(most.same+most.moved) >= need
}

// fromBase reads, from the base's files where they are, the stored chunks a pull still needs
// that the base holds anywhere (#58). The base is not consumed. Each chunk is hash-checked; one
// that cannot be read or does not match is left for the other sources. It returns the bytes read.
func fromBase(ctx context.Context, p rebasePlan, nodes []node, done []bool, handles []*os.File, workers int) (int64, error) {
	type job struct {
		node int32
		file int
		off  int64
	}
	var jobs []job
	for i := range nodes {
		if !nodes[i].stored || done[i] {
			continue
		}
		if f, e, ok := p.find(nodes[i].chunk); ok && e.Len == nodes[i].loc.Length {
			jobs = append(jobs, job{int32(i), f, e.Off})
		}
	}
	if len(jobs) == 0 {
		return 0, nil
	}
	slices.SortFunc(jobs, func(a, b job) int { // read each base file front to back
		if a.file != b.file {
			return a.file - b.file
		}
		return cmp.Compare(a.off, b.off)
	})
	files := make([]*os.File, len(p.files))
	defer func() {
		for _, f := range files {
			if f != nil {
				f.Close()
			}
		}
	}()
	for _, j := range jobs {
		if files[j.file] == nil {
			f, err := os.Open(filepath.Join(p.base.Dir, filepath.FromSlash(p.files[j.file].Path)))
			if err != nil {
				return 0, nil // the base is gone: the other sources serve everything
			}
			files[j.file] = f
		}
	}
	var n atomic.Int64
	var mu sync.Mutex
	var firstErr error
	var wg sync.WaitGroup
	w := max(workers, 1)
	per := (len(jobs) + w - 1) / w
	for a := 0; a < len(jobs); a += per {
		part := jobs[a:min(a+per, len(jobs))]
		wg.Add(1)
		go func() {
			defer wg.Done()
			var buf []byte
			for _, j := range part {
				if ctx.Err() != nil {
					return
				}
				nd := &nodes[j.node]
				if cap(buf) < int(nd.loc.Length) {
					buf = make([]byte, nd.loc.Length)
				}
				b := buf[:nd.loc.Length]
				if _, err := files[j.file].ReadAt(b, j.off); err != nil || digest.Of(b) != nd.chunk {
					continue
				}
				if err := emit(nodes, j.node, b, 0, handles); err != nil {
					mu.Lock()
					firstErr = cmp.Or(firstErr, err)
					mu.Unlock()
					return
				}
				done[j.node] = true
				n.Add(int64(len(b)))
			}
		}()
	}
	wg.Wait()
	return n.Load(), firstErr
}

// sameAt says whether base holds e's chunk at e's offset.
func sameAt(base []Extent, e Extent) bool {
	k, ok := slices.BinarySearchFunc(base, e.Off, func(x Extent, off int64) int {
		switch {
		case x.Off < off:
			return -1
		case x.Off > off:
			return 1
		}
		return 0
	})
	return ok && base[k].Chunk == e.Chunk
}

// rebase moves each paired base file from dir onto the partial file of the new one and marks done
// the stored chunks whose every placement is unchanged. It returns the bytes left in place.
func rebase(p rebasePlan, dir string, m *Manifest, nodes []node, handles []*os.File, done []bool) (int64, error) {
	for i, j := range p.pair {
		if j < 0 {
			continue
		}
		src := filepath.Join(dir, filepath.FromSlash(p.files[j].Path))
		fi, err := os.Lstat(src)
		if err != nil || !fi.Mode().IsRegular() {
			p.pair[i] = -1
			continue
		}
		name := handles[i].Name()
		handles[i].Close()
		handles[i] = nil
		if err := os.Rename(src, name); err != nil {
			p.pair[i] = -1 // the partial file is still there
		} else if err := os.Chmod(name, fi.Mode().Perm()|0o200); err != nil {
			return 0, err
		}
		h, err := os.OpenFile(name, os.O_RDWR, 0)
		if err != nil {
			return 0, err
		}
		handles[i] = h
		if err := h.Truncate(m.Files[i].Size); err != nil {
			return 0, err
		}
	}
	// a chunk is left alone only if every place it goes already holds it
	var unchanged func(id int32, off int64, d digest.Digest) (bool, int64)
	unchanged = func(id int32, off int64, d digest.Digest) (bool, int64) {
		nd := &nodes[id]
		var n int64
		at := func(t target) bool {
			j := p.pair[t.file]
			return j >= 0 && sameAt(p.ext[j], Extent{Off: t.off + off, Chunk: d})
		}
		if nd.hasT {
			if !at(nd.firstT) {
				return false, 0
			}
			n++
			for _, t := range nd.moreT {
				if !at(t) {
					return false, 0
				}
				n++
			}
		}
		if nd.hasP {
			ok, k := unchanged(nd.firstP.node, off+int64(nd.firstP.off), d)
			if !ok {
				return false, 0
			}
			n += k
			for _, e := range nd.moreP {
				ok, k := unchanged(e.node, off+int64(e.off), d)
				if !ok {
					return false, 0
				}
				n += k
			}
		}
		return true, n
	}
	var kept int64
	for i := range nodes {
		if !nodes[i].stored {
			continue
		}
		if ok, n := unchanged(int32(i), 0, nodes[i].chunk); ok && n > 0 {
			done[i] = true
			kept += n * int64(nodes[i].loc.Length)
		}
	}
	return kept, nil
}
