package store

import (
	"context"
	"os"
	"path/filepath"
	"slices"

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
}

// chooseBase returns the base that keeps the most bytes in place, and whether that is at least
// minSame of m's bytes.
func (s *Store) chooseBase(ctx context.Context, m *Manifest, newExt [][]Extent, bases []Base, minSame float64) (rebasePlan, bool) {
	var best rebasePlan
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
		for i, j := range p.pair {
			if j < 0 {
				continue
			}
			if !filepath.IsLocal(filepath.FromSlash(bm.Files[j].Path)) {
				p.pair[i] = -1
				continue
			}
			for _, e := range newExt[i] {
				if sameAt(p.ext[j], e) {
					p.same += int64(e.Len)
				}
			}
		}
		if p.same > best.same {
			best = p
		}
	}
	return best, best.same > 0 && float64(best.same) >= minSame*float64(m.Bytes)
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
