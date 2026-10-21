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
	file int
	off  int64
}

type want struct {
	chunk digest.Digest
	loc   Loc
	dst   []target
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
	wants := map[digest.Digest]*want{}
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
			if err := s.plan(wants, c, target{file: i, off: off}); err != nil {
				return st, fmt.Errorf("%s: %w", f.Path, err)
			}
			off += int64(c.Length)
		}
		if off != f.Size {
			return st, fmt.Errorf("%s: chunks add up to %d bytes, manifest says %d", f.Path, off, f.Size)
		}
	}

	// group by pack and coalesce adjacent chunks into ranges
	all := make([]*want, 0, len(wants))
	for _, w := range wants {
		all = append(all, w)
	}
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i].loc, all[j].loc
		if a.Pack != b.Pack {
			return string(a.Pack[:]) < string(b.Pack[:])
		}
		return a.Offset < b.Offset
	})
	var ranges [][]*want
	for _, w := range all {
		if n := len(ranges); n > 0 {
			last := ranges[n-1]
			first, prev := last[0], last[len(last)-1]
			if prev.loc.Pack == w.loc.Pack && prev.loc.Offset+prev.loc.Stored == w.loc.Offset &&
				int64(w.loc.Offset+w.loc.Stored-first.loc.Offset) <= o.MaxRange {
				ranges[n-1] = append(last, w)
				continue
			}
		}
		ranges = append(ranges, []*want{w})
	}
	st.Ranges = len(ranges)

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	jobs := make(chan []*want)
	var wg sync.WaitGroup
	var readBytes atomic.Int64
	for w := 0; w < o.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range jobs {
				if ctx.Err() != nil {
					continue
				}
				n, err := s.readRange(ctx, r, handles)
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
func (s *Store) plan(wants map[digest.Digest]*want, c ChunkRef, t target) error {
	if w := wants[c.Digest]; w != nil {
		w.dst = append(w.dst, t)
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
		wants[c.Digest] = &want{chunk: c.Digest, loc: loc, dst: []target{t}}
		return nil
	case isComp:
		var n int64
		// A sub-chunk may itself be a composite: another file stored those bytes as a whole chunk.
		// Each level is strictly shorter, so the recursion ends.
		for _, sc := range subs {
			if sc.Length >= c.Length {
				return fmt.Errorf("chunk %s: sub-chunk %s is not shorter than the chunk", c.Digest, sc.Digest)
			}
			if err := s.plan(wants, sc, target{file: t.file, off: t.off + n}); err != nil {
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

func (s *Store) readRange(ctx context.Context, r []*want, handles []*os.File) (int64, error) {
	first, last := r[0].loc, r[len(r)-1].loc
	n := int64(last.Offset+last.Stored) - int64(first.Offset)
	rc, err := s.B.Get(ctx, backend.Key{Kind: backend.Pack, Name: first.Pack.Hex()}, int64(first.Offset), n)
	if err != nil {
		return 0, err
	}
	buf := make([]byte, n)
	_, err = io.ReadFull(rc, buf)
	rc.Close()
	if err != nil {
		return 0, fmt.Errorf("pack %s: %w", first.Pack, err)
	}
	for _, w := range r {
		b, err := codec.Decode(buf[int64(w.loc.Offset)-int64(first.Offset):][:w.loc.Stored], w.loc.Codec, int(w.loc.Length))
		if err != nil {
			return 0, fmt.Errorf("pack %s offset %d: chunk %s: %w", first.Pack, w.loc.Offset, w.chunk, err)
		}
		if digest.Of(b) != w.chunk {
			return 0, fmt.Errorf("pack %s offset %d: chunk %s fails its hash check", first.Pack, w.loc.Offset, w.chunk)
		}
		for _, t := range w.dst {
			if _, err := handles[t.file].WriteAt(b, t.off); err != nil {
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
