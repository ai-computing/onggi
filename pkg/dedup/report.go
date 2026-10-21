// Package dedup measures how much a set of files would shrink in a content-addressed store.
//
// Files are chunked in parallel, then merged in a fixed order (kind, then path) so the "new
// bytes" each file adds are reproducible: a later copy of a source checkpoint shows as zero.
package dedup

import (
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/ai-computing/onggi/pkg/chunker"
	"github.com/ai-computing/onggi/pkg/layout"
)

// Options controls a report.
type Options struct {
	Params     chunker.Params
	Workers    int   // files chunked at once
	MinSize    int64 // skip smaller files
	Pairs      int   // pairwise shares among this many largest files (0: none)
	FixedBlock int   // also measure fixed-size blocks of this many bytes (0: no)
}

// File is one input file's result.
type File struct {
	Path     string      `json:"path"`
	Kind     layout.Kind `json:"kind"`
	Bytes    int64       `json:"bytes"`
	NewBytes int64       `json:"new_bytes"` // bytes not seen in any earlier file
	Chunks   int         `json:"chunks"`
	ids      []int32
	sizes    []int32
}

// Group sums the files of one kind.
type Group struct {
	Kind     layout.Kind `json:"kind"`
	Files    int         `json:"files"`
	Bytes    int64       `json:"bytes"`
	NewBytes int64       `json:"new_bytes"`
	Unique   int64       `json:"unique_bytes"` // distinct bytes within the kind alone
}

// Pair is the share of A's bytes that also occur in B.
type Pair struct {
	A     string  `json:"a"`
	B     string  `json:"b"`
	Share float64 `json:"share_of_a"`
}

// Report is the result.
type Report struct {
	Params      chunker.Params `json:"chunk_params"`
	Bytes       int64          `json:"bytes"`
	UniqueBytes int64          `json:"unique_bytes"`
	Ratio       float64        `json:"ratio"`
	Chunks      int            `json:"chunks"`
	UniqueChunk int            `json:"unique_chunks"`
	Groups      []Group        `json:"groups"`
	Files       []File         `json:"files"`
	Pairs       []Pair         `json:"pairs,omitempty"`
	Fixed       *FixedResult   `json:"fixed_block,omitempty"`
	Skipped     []string       `json:"skipped,omitempty"` // symlinks and unreadable entries
}

// FixedResult is the same measurement with fixed-size blocks, for comparison.
type FixedResult struct {
	Block       int     `json:"block_bytes"`
	UniqueBytes int64   `json:"unique_bytes"`
	Ratio       float64 `json:"ratio"`
}

// Collect walks paths and returns regular files in report order. Symlinks are not followed
// (a model hub's snapshot links would count its blobs twice) and are listed as skipped.
func Collect(paths []string, minSize int64) (files []File, skipped []string, err error) {
	seen := map[string]bool{}
	for _, root := range paths {
		err = filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
			if werr != nil {
				skipped = append(skipped, p)
				return nil
			}
			if d.Type()&fs.ModeSymlink != 0 {
				skipped = append(skipped, p)
				return nil
			}
			if !d.Type().IsRegular() {
				return nil
			}
			info, ierr := d.Info()
			if ierr != nil || info.Size() < minSize {
				return nil
			}
			abs, _ := filepath.Abs(p)
			if seen[abs] {
				return nil
			}
			seen[abs] = true
			files = append(files, File{Path: p, Kind: layout.Of(p), Bytes: info.Size()})
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
	}
	order := map[layout.Kind]int{}
	for i, k := range layout.All {
		order[k] = i
	}
	sort.SliceStable(files, func(i, j int) bool {
		if files[i].Kind != files[j].Kind {
			return order[files[i].Kind] < order[files[j].Kind]
		}
		return files[i].Path < files[j].Path
	})
	return files, skipped, nil
}

type chunkList struct {
	hashes [][32]byte
	sizes  []int32
	fixed  [][32]byte
	err    error
}

func chunkFile(path string, o Options) (cl chunkList) {
	f, err := os.Open(path)
	if err != nil {
		cl.err = err
		return
	}
	defer f.Close()
	var r io.Reader = f
	var fixed *fixedHasher
	if o.FixedBlock > 0 {
		fixed = &fixedHasher{block: o.FixedBlock}
		r = io.TeeReader(f, fixed)
	}
	c := chunker.New(r, o.Params)
	for {
		ch, err := c.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			cl.err = err
			return
		}
		cl.hashes = append(cl.hashes, sha256.Sum256(ch))
		cl.sizes = append(cl.sizes, int32(len(ch)))
	}
	if fixed != nil {
		cl.fixed = fixed.done()
	}
	return
}

// fixedHasher hashes the stream in fixed-size blocks as it passes through.
type fixedHasher struct {
	block  int
	cur    []byte
	hashes [][32]byte
}

func (h *fixedHasher) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		take := h.block - len(h.cur)
		if take > len(p) {
			take = len(p)
		}
		h.cur = append(h.cur, p[:take]...)
		p = p[take:]
		if len(h.cur) == h.block {
			h.hashes = append(h.hashes, sha256.Sum256(h.cur))
			h.cur = h.cur[:0]
		}
	}
	return n, nil
}

func (h *fixedHasher) done() [][32]byte {
	if len(h.cur) > 0 {
		h.hashes = append(h.hashes, sha256.Sum256(h.cur))
	}
	return h.hashes
}

// Run chunks files and builds the report. progress, if not nil, is called after each file.
func Run(files []File, skipped []string, o Options, progress func(done, total int, f File)) (*Report, error) {
	if err := o.Params.Validate(); err != nil {
		return nil, err
	}
	if o.Workers < 1 {
		o.Workers = 1
	}
	lists := make([]chunkList, len(files))
	ready := make([]chan struct{}, len(files))
	for i := range ready {
		ready[i] = make(chan struct{})
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < o.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				lists[i] = chunkFile(files[i].Path, o)
				close(ready[i])
			}
		}()
	}
	go func() {
		for i := range files {
			jobs <- i
		}
		close(jobs)
		wg.Wait()
	}()

	rep := &Report{Params: o.Params, Skipped: skipped}
	index := map[[32]byte]int32{}
	var fixedSeen map[[32]byte]bool
	var fixedBytes, fixedUnique int64
	if o.FixedBlock > 0 {
		fixedSeen = map[[32]byte]bool{}
	}
	groups := map[layout.Kind]*Group{}
	groupSeen := map[layout.Kind]map[int32]bool{}
	for i := range files { // merge in report order, as each file becomes ready
		<-ready[i]
		cl := lists[i]
		if cl.err != nil {
			return nil, cl.err
		}
		f := &files[i]
		g := groups[f.Kind]
		if g == nil {
			g = &Group{Kind: f.Kind}
			groups[f.Kind] = g
			groupSeen[f.Kind] = map[int32]bool{}
		}
		f.ids = make([]int32, len(cl.hashes))
		f.sizes = cl.sizes
		for j, h := range cl.hashes {
			id, ok := index[h]
			if !ok {
				id = int32(len(index))
				index[h] = id
				f.NewBytes += int64(cl.sizes[j])
				rep.UniqueBytes += int64(cl.sizes[j])
			}
			if !groupSeen[f.Kind][id] {
				groupSeen[f.Kind][id] = true
				g.Unique += int64(cl.sizes[j])
			}
			f.ids[j] = id
		}
		for j, h := range cl.fixed {
			size := int64(o.FixedBlock)
			if j == len(cl.fixed)-1 {
				size = f.Bytes - int64(o.FixedBlock)*int64(len(cl.fixed)-1)
			}
			fixedBytes += size
			if !fixedSeen[h] {
				fixedSeen[h] = true
				fixedUnique += size
			}
		}
		f.Chunks = len(cl.hashes)
		g.Files++
		g.Bytes += f.Bytes
		g.NewBytes += f.NewBytes
		rep.Bytes += f.Bytes
		rep.Chunks += f.Chunks
		lists[i] = chunkList{} // free the hashes
		if progress != nil {
			progress(i+1, len(files), *f)
		}
	}
	rep.UniqueChunk = len(index)
	rep.Ratio = ratio(rep.Bytes, rep.UniqueBytes)
	for _, k := range layout.All {
		if g := groups[k]; g != nil {
			rep.Groups = append(rep.Groups, *g)
		}
	}
	if o.FixedBlock > 0 {
		rep.Fixed = &FixedResult{Block: o.FixedBlock, UniqueBytes: fixedUnique, Ratio: ratio(fixedBytes, fixedUnique)}
	}
	rep.Pairs = pairs(files, o.Pairs)
	rep.Files = files
	return rep, nil
}

func ratio(total, unique int64) float64 {
	if unique == 0 {
		return 0
	}
	return float64(total) / float64(unique)
}

// pairs reports, for the n largest files, which share of each file's bytes occurs in each other.
func pairs(files []File, n int) []Pair {
	if n < 2 {
		return nil
	}
	idx := make([]int, len(files))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return files[idx[a]].Bytes > files[idx[b]].Bytes })
	if n > len(idx) {
		n = len(idx)
	}
	sets := make([]map[int32]bool, n)
	for k := 0; k < n; k++ {
		sets[k] = map[int32]bool{}
		for _, id := range files[idx[k]].ids {
			sets[k][id] = true
		}
	}
	var out []Pair
	for a := 0; a < n; a++ {
		fa := files[idx[a]]
		for b := 0; b < n; b++ {
			if a == b {
				continue
			}
			var shared int64
			for j, id := range fa.ids {
				if sets[b][id] {
					shared += int64(fa.sizes[j])
				}
			}
			share := 0.0
			if fa.Bytes > 0 {
				share = float64(shared) / float64(fa.Bytes)
			}
			out = append(out, Pair{A: fa.Path, B: files[idx[b]].Path, Share: share})
		}
	}
	return out
}
