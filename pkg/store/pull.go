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

// want is one distinct stored chunk and where its bytes go. Most chunks go to one place, so the
// first target is inline; a two-level artifact has millions of wants (#17).
type want struct {
	chunk digest.Digest
	loc   Loc
	first target
	more  []target
}

// planner collects the wants of one pull: a dense slice and an index by digest.
type planner struct {
	idx map[digest.Digest]int32
	ws  []want
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
	pl := &planner{idx: map[digest.Digest]int32{}}
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
		list, err := s.readList(ctx, f.List)
		if err != nil {
			return st, fmt.Errorf("%s: %w", f.Path, err)
		}
		var off int64
		for _, c := range list {
			if err := s.plan(pl, c, target{file: int32(i), off: off}); err != nil {
				return st, fmt.Errorf("%s: %w", f.Path, err)
			}
			off += int64(c.Length)
		}
		if off != f.Size {
			return st, fmt.Errorf("%s: chunks add up to %d bytes, manifest says %d", f.Path, off, f.Size)
		}
	}
	ws := pl.ws
	pl.idx = nil // the digest index is not needed for reading

	// group by pack and coalesce adjacent chunks into ranges of the sorted order
	order := make([]int32, len(ws))
	for i := range order {
		order[i] = int32(i)
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := ws[order[i]].loc, ws[order[j]].loc
		if a.Pack != b.Pack {
			return a.Pack < b.Pack
		}
		return a.Offset < b.Offset
	})
	type span struct{ a, b int } // order[a:b]
	var ranges []span
	for k, i := range order {
		l := ws[i].loc
		if n := len(ranges); n > 0 {
			r := &ranges[n-1]
			first, prev := ws[order[r.a]].loc, ws[order[r.b-1]].loc
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
				n, err := s.readRange(ctx, ws, order[r.a:r.b], handles, &buf)
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

// plan records that chunk c goes to t: as stored bytes, or through its sub-chunks.
func (s *Store) plan(pl *planner, c ChunkRef, t target) error {
	if i, ok := pl.idx[c.Digest]; ok {
		pl.ws[i].more = append(pl.ws[i].more, t)
		return nil
	}
	s.mu.RLock()
	loc, ok := s.index[c.Digest]
	subs, isComp := s.comps[c.Digest]
	s.mu.RUnlock()
	switch {
	case ok:
		if loc.Length != c.Length {
			return fmt.Errorf("chunk %s length %d in the list, %d in the index", c.Digest, c.Length, loc.Length)
		}
		pl.idx[c.Digest] = int32(len(pl.ws))
		pl.ws = append(pl.ws, want{chunk: c.Digest, loc: loc, first: t})
		return nil
	case isComp:
		var n int64
		// A sub-chunk may itself be a composite: another file stored those bytes as a whole chunk.
		// Each level is strictly shorter, so the recursion ends.
		for _, sc := range subs {
			if sc.Length >= c.Length {
				return fmt.Errorf("chunk %s: sub-chunk %s is not shorter than the chunk", c.Digest, sc.Digest)
			}
			if err := s.plan(pl, sc, target{file: t.file, off: t.off + n}); err != nil {
				return fmt.Errorf("chunk %s: %w", c.Digest, err)
			}
			n += int64(sc.Length)
		}
		if n != int64(c.Length) {
			return fmt.Errorf("chunk %s: sub-chunks add up to %d bytes, want %d", c.Digest, n, c.Length)
		}
		return nil
	}
	return fmt.Errorf("chunk %s is not in the store", c.Digest)
}

// readRange reads one range of a pack into *buf (reused across calls of one worker), decodes and
// checks each chunk, and writes it to its targets.
func (s *Store) readRange(ctx context.Context, ws []want, ids []int32, handles []*os.File, buf *[]byte) (int64, error) {
	first, last := ws[ids[0]].loc, ws[ids[len(ids)-1]].loc
	pack := s.packName(first.Pack)
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
		w := &ws[i]
		raw, err := codec.Decode(b[int64(w.loc.Offset)-int64(first.Offset):][:w.loc.Stored], w.loc.Codec, int(w.loc.Length))
		if err != nil {
			return 0, fmt.Errorf("pack %s offset %d: chunk %s: %w", pack, w.loc.Offset, w.chunk, err)
		}
		if digest.Of(raw) != w.chunk {
			return 0, fmt.Errorf("pack %s offset %d: chunk %s fails its hash check", pack, w.loc.Offset, w.chunk)
		}
		if _, err := handles[w.first.file].WriteAt(raw, w.first.off); err != nil {
			return 0, err
		}
		for _, t := range w.more {
			if _, err := handles[t.file].WriteAt(raw, t.off); err != nil {
				return 0, err
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
