package dedup

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/ai-computing/onggi/pkg/chunker"
)

// Placement classes (#74). Each names how the bytes a file shares with the rest of the set and
// with the reference files can be found, which bytes alone reveal (ADR-0009: no format is read):
//
//   - referenced: at least RefShare of the file's bytes are in the reference files (a checkpoint)
//     at coarse chunks; any chunking finds them there.
//   - embedded-rearranged: fine chunks find at least RearrangedGap more of the file's bytes
//     elsewhere than coarse chunks do; the shared bytes lie in runs shorter than a coarse chunk.
//   - embedded-shifted: coarse chunks find at least ShiftedShare of the file's bytes elsewhere;
//     they are copied whole, at whatever offsets.
//   - new: none of the above.
//
// "Elsewhere" is any other file of the set or a reference file. The rules apply in this order.
const (
	Referenced          = "referenced"
	EmbeddedRearranged  = "embedded-rearranged"
	EmbeddedShifted     = "embedded-shifted"
	New                 = "new"
	DefaultRefShare     = 0.95
	DefaultRearrangeGap = 0.25
	DefaultShiftedShare = 0.5
)

// DefaultFine is the fine level: the store's sub-chunks (1 / 4 / 16 KiB).
var DefaultFine = chunker.Params{Min: 1 << 10, Avg: 4 << 10, Max: 16 << 10}

// Classify configures the placement report.
type Classify struct {
	Fine          chunker.Params `json:"fine_params"` // each coarse chunk is split at these
	References    []string       `json:"references,omitempty"`
	RefShare      float64        `json:"ref_share"`
	RearrangedGap float64        `json:"rearranged_gap"`
	ShiftedShare  float64        `json:"shifted_share"`
	RefFiles      int            `json:"reference_files"`
	RefBytes      int64          `json:"reference_bytes"`
}

// Shares are the shares of a file's bytes found in the reference files (coarse chunks) and
// anywhere else (coarse and fine chunks).
type Shares struct {
	Ref    float64 `json:"ref"`
	Coarse float64 `json:"coarse"`
	Fine   float64 `json:"fine"`
}

// Class applies the rules above.
func (c *Classify) Class(s Shares) string {
	switch {
	case s.Ref >= c.RefShare:
		return Referenced
	case s.Fine-s.Coarse >= c.RearrangedGap:
		return EmbeddedRearranged
	case s.Coarse >= c.ShiftedShare:
		return EmbeddedShifted
	}
	return New
}

// fineKey names a fine chunk by the first 8 bytes of its SHA-256: a report needs no more, and it
// halves the memory of the fine index (a collision only moves bytes between shares).
func fineKey(h [32]byte) uint64 { return binary.LittleEndian.Uint64(h[:8]) }

// splitFine hashes the fine chunks of one coarse chunk.
func splitFine(p chunker.Params, ch []byte, cl *chunkList) {
	p.Split(ch, func(sub []byte) error {
		cl.fine = append(cl.fine, fineKey(sha256.Sum256(sub)))
		cl.fineSizes = append(cl.fineSizes, int32(len(sub)))
		return nil
	})
}

// refSets chunks the reference files and returns their coarse and fine chunk sets.
func refSets(c *Classify, o Options) (map[[32]byte]struct{}, map[uint64]struct{}, error) {
	var paths []string
	seen := map[string]bool{}
	for _, root := range c.References {
		// references may be a model hub snapshot, whose files are links to blobs: follow them
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			if d.IsDir() {
				return nil
			}
			info, err := os.Stat(p)
			if err != nil || !info.Mode().IsRegular() {
				return nil
			}
			real, err := filepath.EvalSymlinks(p)
			if err != nil {
				return nil
			}
			if !seen[real] {
				seen[real] = true
				paths = append(paths, real)
				c.RefFiles++
				c.RefBytes += info.Size()
			}
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
	}
	coarse, fine := map[[32]byte]struct{}{}, map[uint64]struct{}{}
	var mu sync.Mutex
	var firstErr error
	jobs := make(chan string)
	var wg sync.WaitGroup
	for w := 0; w < max(o.Workers, 1); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range jobs {
				cl := chunkFile(p, Options{Params: o.Params, Classify: o.Classify})
				mu.Lock()
				if cl.err != nil && firstErr == nil {
					firstErr = cl.err
				}
				for _, h := range cl.hashes {
					coarse[h] = struct{}{}
				}
				for _, h := range cl.fine {
					fine[h] = struct{}{}
				}
				mu.Unlock()
			}
		}()
	}
	for _, p := range paths {
		jobs <- p
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return nil, nil, firstErr
	}
	if len(c.References) > 0 && len(paths) == 0 {
		return nil, nil, errors.New("no reference files found")
	}
	return coarse, fine, nil
}

// placement tracks, while files are merged in report order, which chunks occur in more than one
// file of the set, so each file's shares count only bytes found elsewhere.
type placement struct {
	c         *Classify
	refCoarse map[[32]byte]struct{}
	refFine   map[uint64]struct{}
	owner     []int32          // per coarse chunk id: the one file holding it, or -1 for several
	fineOwner map[uint64]int32 // the same for fine chunks
	inRef     [][]bool         // per file and coarse chunk: in the reference files
	refBytes  []int64
	fine      [][]uint64
	fineSizes [][]int32
}

func newPlacement(c *Classify, o Options, files int) (*placement, error) {
	rc, rf, err := refSets(c, o)
	if err != nil {
		return nil, err
	}
	return &placement{c: c, refCoarse: rc, refFine: rf, fineOwner: map[uint64]int32{},
		inRef: make([][]bool, files), refBytes: make([]int64, files),
		fine: make([][]uint64, files), fineSizes: make([][]int32, files)}, nil
}

// add records file i's chunks; ids are its coarse chunk ids, new ids are numbered in order.
func (p *placement) add(i int, cl chunkList, ids []int32) {
	in := make([]bool, len(cl.hashes))
	for j, h := range cl.hashes {
		if _, ok := p.refCoarse[h]; ok {
			in[j] = true
			p.refBytes[i] += int64(cl.sizes[j])
		}
		id := ids[j]
		switch {
		case int(id) == len(p.owner):
			p.owner = append(p.owner, int32(i))
		case p.owner[id] != int32(i):
			p.owner[id] = -1
		}
	}
	p.inRef[i] = in
	for _, h := range cl.fine {
		if o, ok := p.fineOwner[h]; !ok {
			p.fineOwner[h] = int32(i)
		} else if o != int32(i) {
			p.fineOwner[h] = -1
		}
	}
	p.fine[i], p.fineSizes[i] = cl.fine, cl.fineSizes
}

// finish sets each file's shares and class.
func (p *placement) finish(files []File) {
	for i := range files {
		f := &files[i]
		if f.Bytes == 0 {
			continue
		}
		var coarse, fine int64
		for j, id := range f.ids {
			if p.owner[id] < 0 || p.inRef[i][j] {
				coarse += int64(f.sizes[j])
			}
		}
		for j, h := range p.fine[i] {
			_, ref := p.refFine[h]
			if p.fineOwner[h] < 0 || ref {
				fine += int64(p.fineSizes[i][j])
			}
		}
		b := float64(f.Bytes)
		f.Shares = &Shares{Ref: float64(p.refBytes[i]) / b, Coarse: float64(coarse) / b, Fine: float64(fine) / b}
		f.Class = p.c.Class(*f.Shares)
		p.fine[i], p.fineSizes[i], p.inRef[i] = nil, nil, nil
	}
}
