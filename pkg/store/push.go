package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ai-computing/onggi/pkg/backend"
	"github.com/ai-computing/onggi/pkg/chunker"
	"github.com/ai-computing/onggi/pkg/codec"
	"github.com/ai-computing/onggi/pkg/digest"
	"github.com/ai-computing/onggi/pkg/layout"
)

// PushOptions controls a push.
type PushOptions struct {
	Workers  int               // files chunked at once
	Uploads  int               // packs uploaded at once
	TempDir  string            // where packs are assembled before upload
	Labels   map[string]string // stored in the manifest
	Profile  *Profile          // optional exact compatibility declaration
	Compress codec.Mode        // how new chunks are stored; "" means none
	Progress func(PushStats)   // called after each file
}

// PushStats reports a push.
type PushStats struct {
	Files      int           `json:"files"`
	Bytes      int64         `json:"bytes"`       // logical bytes read
	NewBytes   int64         `json:"new_bytes"`   // chunk bytes the store did not have (raw)
	Packs      int           `json:"packs"`       // packs written
	PackBytes  int64         `json:"pack_bytes"`  // bytes uploaded in packs
	Skipped    []string      `json:"skipped"`     // symlinks and special files
	Duration   time.Duration `json:"duration_ns"` //
	FilesTotal int           `json:"files_total"`
	BytesTotal int64         `json:"bytes_total"`
}

type pending struct {
	data    []byte // stored form
	raw     uint32
	codec   codec.ID
	chunk   digest.Digest
	members []member // a frame's chunks (#52); empty: data is the one chunk above
}

type sealed struct {
	path    string
	digest  digest.Digest
	size    int64
	entries []indexEntry
}

// Push stores the directory tree under root and returns the manifest digest. Only chunks the
// store does not hold are uploaded.
func (s *Store) Push(ctx context.Context, root string, o PushOptions) (digest.Digest, PushStats, error) {
	if o.Profile != nil {
		if err := o.Profile.Validate(); err != nil {
			return digest.Digest{}, PushStats{}, err
		}
	}
	start := time.Now()
	// a lease keeps GC from sweeping chunks this push deduplicates against (ADR-0005)
	release, err := s.Lease(ctx, "push")
	if err != nil {
		return digest.Digest{}, PushStats{}, err
	}
	defer release()
	if err := s.LoadIndex(ctx); err != nil {
		return digest.Digest{}, PushStats{}, err
	}
	if o.Workers < 1 {
		o.Workers = 4
	}
	if o.Uploads < 1 {
		o.Uploads = 4
	}
	if o.TempDir == "" {
		o.TempDir = os.TempDir()
	}
	files, skipped, err := walk(root)
	if err != nil {
		return digest.Digest{}, PushStats{}, err
	}
	var st PushStats
	st.Skipped = skipped
	st.FilesTotal = len(files)
	for _, f := range files {
		st.BytesTotal += f.Size
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	fail := func(err error) { cancel(err) }

	// chunks this push has queued for a pack and not yet indexed; an entry leaves once its pack's
	// index is loaded, so the set holds only chunks in flight (#17)
	claimed := map[digest.Digest]bool{}
	var claimMu sync.Mutex
	claim := func(d digest.Digest) bool {
		if s.has(d) {
			return false
		}
		claimMu.Lock()
		defer claimMu.Unlock()
		if claimed[d] {
			return false
		}
		claimed[d] = true
		return true
	}

	var comps []composite // chunks this push stores as sub-chunks, not yet published
	var compMu sync.Mutex
	addComp := func(c composite) {
		compMu.Lock()
		comps = append(comps, c)
		compMu.Unlock()
	}
	// publishReady writes the composites whose sub-chunks are all indexed (durable), so pending
	// composites do not pile up until the end of the push.
	publishReady := func() error {
		compMu.Lock()
		if len(comps) < compFlushAt {
			compMu.Unlock()
			return nil
		}
		var ready, wait []composite
		for _, c := range comps {
			ok := true
			for _, sc := range c.Subs {
				if !s.has(sc.Digest) {
					ok = false
					break
				}
			}
			if ok {
				ready = append(ready, c)
			} else {
				wait = append(wait, c)
			}
		}
		comps = wait
		compMu.Unlock()
		if err := s.putComposites(ctx, ready); err != nil {
			return err
		}
		claimMu.Lock()
		for _, c := range ready {
			delete(claimed, c.Chunk)
		}
		claimMu.Unlock()
		return nil
	}

	chunks := make(chan pending, 64)
	packs := make(chan sealed, o.Uploads)
	var newBytes, packBytes atomic.Int64
	var packCount atomic.Int64

	// packer: one goroutine appends new chunks to the current pack file.
	var packerWG sync.WaitGroup
	packerWG.Add(1)
	go func() {
		defer packerWG.Done()
		defer close(packs)
		var f *os.File
		var h hash.Hash
		var size int64
		var entries []indexEntry
		seal := func() {
			if f == nil {
				return
			}
			var d digest.Digest
			copy(d[:], h.Sum(nil))
			name := f.Name()
			if err := f.Close(); err != nil {
				fail(err)
			}
			select {
			case packs <- sealed{path: name, digest: d, size: size, entries: entries}:
			case <-ctx.Done():
				os.Remove(name)
			}
			f, h, size, entries = nil, nil, 0, nil
		}
		for p := range chunks {
			if ctx.Err() != nil {
				continue // drain
			}
			if f == nil {
				var err error
				if f, err = os.CreateTemp(o.TempDir, "onggi-pack-*"); err != nil {
					fail(err)
					continue
				}
				h = sha256.New()
			}
			if _, err := f.Write(p.data); err != nil {
				fail(err)
				continue
			}
			h.Write(p.data)
			if len(p.members) == 0 {
				entries = append(entries, indexEntry{Chunk: p.chunk, Loc: Loc{Offset: uint32(size), Stored: uint32(len(p.data)), Length: p.raw, Codec: p.codec}})
			}
			for _, m := range p.members {
				entries = append(entries, indexEntry{Chunk: m.chunk, Loc: Loc{Offset: uint32(size), Stored: uint32(len(p.data)),
					Length: m.length, Codec: p.codec, Inner: m.inner, Frame: p.raw}})
			}
			size += int64(len(p.data))
			if size >= s.Config.PackSize {
				seal()
			}
		}
		if ctx.Err() == nil {
			seal()
		} else if f != nil {
			f.Close()
			os.Remove(f.Name())
		}
	}()

	// uploaders: pack, then its index; only then are its chunks visible to later pushes.
	var upWG sync.WaitGroup
	for i := 0; i < o.Uploads; i++ {
		upWG.Add(1)
		go func() {
			defer upWG.Done()
			for p := range packs {
				if ctx.Err() == nil {
					if err := s.upload(ctx, p); err != nil {
						fail(err)
					} else {
						packCount.Add(1)
						packBytes.Add(p.size)
						claimMu.Lock()
						for _, e := range p.entries {
							delete(claimed, e.Chunk) // indexed now: s.has answers for it
						}
						claimMu.Unlock()
						if err := publishReady(); err != nil {
							fail(err)
						}
					}
				}
				os.Remove(p.path)
			}
		}()
	}

	// chunkers: files in parallel; each keeps its own ordered chunk list.
	lists := make([][]ChunkRef, len(files))
	jobs := make(chan int)
	var chWG sync.WaitGroup
	var statMu sync.Mutex
	for w := 0; w < o.Workers; w++ {
		chWG.Add(1)
		go func() {
			defer chWG.Done()
			for i := range jobs {
				if ctx.Err() != nil {
					continue
				}
				list, nb, err := s.chunkFile(ctx, files[i].abs, s.subChunks(files[i].rel), o.Compress, claim, addComp, chunks)
				if err != nil {
					fail(fmt.Errorf("%s: %w", files[i].rel, err))
					continue
				}
				lists[i] = list
				newBytes.Add(nb)
				statMu.Lock()
				st.Files++
				st.Bytes += files[i].Size
				if o.Progress != nil {
					cp := st
					cp.NewBytes = newBytes.Load()
					cp.Packs = int(packCount.Load())
					o.Progress(cp)
				}
				statMu.Unlock()
			}
		}()
	}
	for i := range files {
		jobs <- i
	}
	close(jobs)
	chWG.Wait()
	close(chunks)
	packerWG.Wait()
	upWG.Wait()
	if err := context.Cause(ctx); err != nil {
		return digest.Digest{}, st, err
	}

	// composites after every pack (their sub-chunks are durable), before the lists that name them
	if err := s.putComposites(ctx, comps); err != nil {
		return digest.Digest{}, st, err
	}

	// chunk lists, then the manifest: the commit point for the artifact's content.
	m := Manifest{Schema: "onggi.manifest.v1", Created: time.Now().UTC(), Chunker: s.Config.Chunker, Labels: o.Labels, Profile: o.Profile}
	for i, f := range files {
		data := encodeList(lists[i])
		d := digest.Of(data)
		if err := s.B.Put(ctx, backend.Key{Kind: backend.List, Name: d.Hex()}, bytes.NewReader(data), int64(len(data))); err != nil {
			return digest.Digest{}, st, err
		}
		m.Files = append(m.Files, File{Path: f.rel, Size: f.Size, Mode: uint32(f.Mode.Perm()), Kind: layout.Of(f.rel), List: d})
		m.Bytes += f.Size
	}
	claimed = nil // no longer needed; the locator's own map replaces it
	ld, err := s.putLocator(ctx, lists, o.TempDir)
	if err != nil {
		return digest.Digest{}, st, fmt.Errorf("locator: %w", err)
	}
	m.Locator = &ld
	data, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return digest.Digest{}, st, err
	}
	md := digest.Of(data)
	if err := s.B.Put(ctx, backend.Key{Kind: backend.Manifest, Name: md.Hex()}, bytes.NewReader(data), int64(len(data))); err != nil {
		return digest.Digest{}, st, err
	}
	st.NewBytes, st.Packs, st.PackBytes = newBytes.Load(), int(packCount.Load()), packBytes.Load()
	st.Duration = time.Since(start)
	return md, st, nil
}

// putLocator writes the locator to a temporary file, names it by its digest and uploads it.
func (s *Store) putLocator(ctx context.Context, lists [][]ChunkRef, dir string) (digest.Digest, error) {
	f, err := os.CreateTemp(dir, "onggi-locator-*")
	if err != nil {
		return digest.Digest{}, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	size, err := s.writeLocator(lists, f)
	if err != nil {
		return digest.Digest{}, err
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(f, 0, size)); err != nil {
		return digest.Digest{}, err
	}
	var d digest.Digest
	copy(d[:], h.Sum(nil))
	if err := s.B.Put(ctx, backend.Key{Kind: backend.List, Name: d.Hex()}, io.NewSectionReader(f, 0, size), size); err != nil {
		return digest.Digest{}, err
	}
	return d, nil
}

func (s *Store) upload(ctx context.Context, p sealed) error {
	f, err := os.Open(p.path)
	if err != nil {
		return err
	}
	err = s.B.Put(ctx, backend.Key{Kind: backend.Pack, Name: p.digest.Hex()}, f, p.size)
	f.Close()
	if err != nil {
		return fmt.Errorf("pack %s: %w", p.digest, err)
	}
	idx := encodeIndex(p.digest, p.entries)
	name := digest.Of(idx).Hex()
	if err := s.B.Put(ctx, backend.Key{Kind: backend.Index, Name: name}, bytes.NewReader(idx), int64(len(idx))); err != nil {
		return fmt.Errorf("index of pack %s: %w", p.digest, err)
	}
	return s.addIndex(name, idx)
}

// compTableMax bounds one composite table object (about 40 MB at 64 KiB chunks of 4 KiB subs).
const compTableMax = 64 << 10

// compFlushAt is how many pending composites make an uploader try to publish the ready ones.
var compFlushAt = 16 << 10 // a variable so tests can lower it

func (s *Store) putComposites(ctx context.Context, cs []composite) error {
	for len(cs) > 0 {
		n := min(len(cs), compTableMax)
		data := encodeComposites(cs[:n])
		name := digest.Of(data).Hex()
		if err := s.B.Put(ctx, backend.Key{Kind: backend.Index, Name: name}, bytes.NewReader(data), int64(len(data))); err != nil {
			return fmt.Errorf("composite table: %w", err)
		}
		s.addComposites(cs[:n])
		cs = cs[n:]
	}
	return nil
}

func (s *Store) chunkFile(ctx context.Context, path string, twoLevel bool, mode codec.Mode, claim func(digest.Digest) bool, addComp func(composite), out chan<- pending) ([]ChunkRef, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	c := chunker.New(f, s.Config.Chunker)
	var list []ChunkRef
	var nb int64
	var fb *frameBuilder // new chunks of a sub-chunked file go out in compression frames (#52)
	if twoLevel && s.Config.Frame > 0 {
		fb = &frameBuilder{limit: int(s.Config.Frame), mode: mode}
	}
	for {
		ch, err := c.Next()
		if errors.Is(err, io.EOF) {
			if fb != nil {
				if err := fb.flush(ctx, out); err != nil {
					return nil, 0, err
				}
			}
			return list, nb, nil
		}
		if err != nil {
			return nil, 0, err
		}
		d := digest.Of(ch)
		list = append(list, ChunkRef{Digest: d, Length: uint32(len(ch))})
		if !claim(d) {
			continue
		}
		if !twoLevel {
			n, err := send(ctx, ch, d, mode, out)
			if err != nil {
				return nil, 0, err
			}
			nb += n
			continue
		}
		// two levels: store the new chunk as sub-chunks, sending only the ones the store lacks
		var subs []ChunkRef
		err = s.Config.Sub.Split(ch, func(sub []byte) error {
			sd := digest.Of(sub)
			subs = append(subs, ChunkRef{Digest: sd, Length: uint32(len(sub))})
			if sd == d || claim(sd) { // sd == d: the whole chunk is one sub-chunk; this push owns d already
				if fb != nil {
					n, err := fb.add(ctx, sub, sd, out)
					nb += n
					return err
				}
				n, err := send(ctx, sub, sd, mode, out)
				if err != nil {
					return err
				}
				nb += n
			}
			return nil
		})
		if err != nil {
			return nil, 0, err
		}
		if len(subs) == 1 && subs[0].Digest == d {
			continue // one sub-chunk equal to the chunk: it is stored as bytes under d
		}
		addComp(composite{Chunk: d, Subs: subs})
	}
}

// send queues one new chunk for a pack, compressed per mode, and returns its raw size.
func send(ctx context.Context, raw []byte, d digest.Digest, mode codec.Mode, out chan<- pending) (int64, error) {
	data, id := codec.Encode(raw, mode)
	if id == codec.None {
		data = append([]byte(nil), raw...) // the chunker reuses its buffer
	}
	select {
	case out <- pending{data: data, raw: uint32(len(raw)), codec: id, chunk: d}:
		return int64(len(raw)), nil
	case <-ctx.Done():
		return 0, context.Cause(ctx)
	}
}

type walked struct {
	abs, rel string
	Size     int64
	Mode     fs.FileMode
}

// walk lists regular files under root in path order. Symlinks are not followed: an artifact is
// what is physically there.
func walk(root string) (files []walked, skipped []string, err error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, nil, err
	}
	if !info.IsDir() {
		return []walked{{abs: root, rel: filepath.Base(root), Size: info.Size(), Mode: info.Mode()}}, nil, nil
	}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if !d.Type().IsRegular() {
			skipped = append(skipped, rel)
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		files = append(files, walked{abs: p, rel: rel, Size: info.Size(), Mode: info.Mode()})
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	return files, skipped, err
}
