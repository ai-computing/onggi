// Package casync materializes artifacts from a casync-format chunk store (desync), and adds what a
// stateless tool cannot do: it knows which sibling on the node is idle, aligned and unchanged since
// it was written, and turns that sibling into the requested artifact by writing only the chunks that
// differ, without reading the ones it keeps.
//
// Layout under Engine.IndexDir:
//
//	<manifest hex>/artifact.json   files of the artifact: path, size, mode, index file
//	<manifest hex>/<i>.caibx       casync index of file i
//
// The manifest digest is the SHA-256 of artifact.json. Chunks live in a desync local store
// (one zstd-compressed file per chunk, named by its SHA512/256). Engine.AttestDir records, for every
// file this engine wrote, the inode, size and modification time it had when the write completed;
// a later Keep trusts the chunks it leaves in place only if the file still has them (state-aware
// in-place), and otherwise reads them back and checks each digest.
package casync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/folbricht/desync"

	"github.com/ai-computing/onggi/pkg/csi"
	"github.com/ai-computing/onggi/pkg/digest"
	"github.com/ai-computing/onggi/pkg/store"
)

// Chunk sizes of desync's defaults (16/64/256 KiB).
const (
	MinChunk = 16 << 10
	AvgChunk = 64 << 10
	MaxChunk = 256 << 10
)

// Engine reads and writes artifacts in casync format.
type Engine struct {
	Store     desync.LocalStore
	IndexDir  string
	AttestDir string // "" : never trust kept chunks, always read them back
	Workers   int
	MinSame   float64 // share of bytes at the same offset needed to take a base (0: store.DefaultMinSame)
	// VerifyKept forces reading back kept chunks even when the base is attested unchanged.
	VerifyKept bool
	// Durable syncs every written file before the pull returns (default true via New).
	Durable bool
	// Logf, if set, receives one line per phase with its duration.
	Logf func(format string, args ...any)
}

// New opens the chunk store at storeDir and the index and attestation directories.
func New(storeDir, indexDir, attestDir string, workers int) (*Engine, error) {
	for _, d := range []string{storeDir, indexDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	if attestDir != "" {
		if err := os.MkdirAll(attestDir, 0o755); err != nil {
			return nil, err
		}
	}
	s, err := desync.NewLocalStore(storeDir, desync.StoreOptions{N: max(workers, 1)})
	if err != nil {
		return nil, err
	}
	return &Engine{Store: s, IndexDir: indexDir, AttestDir: attestDir, Workers: max(workers, 1), Durable: true}, nil
}

// FileEntry is one file of an artifact.
type FileEntry struct {
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	Mode  uint32 `json:"mode"`
	Index string `json:"index"`
}

// Artifact is the content of artifact.json.
type Artifact struct {
	Files []FileEntry `json:"files"`
}

func (e *Engine) logf(format string, args ...any) {
	if e.Logf != nil {
		e.Logf(format, args...)
	}
}

// Publish chunks every regular file under dir into the store and writes the artifact's indexes.
// It returns the manifest digest.
func (e *Engine) Publish(ctx context.Context, dir string) (digest.Digest, error) {
	var paths []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			rel, _ := filepath.Rel(dir, p)
			paths = append(paths, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return digest.Digest{}, err
	}
	sort.Strings(paths)
	tmp, err := os.MkdirTemp(e.IndexDir, ".publish-")
	if err != nil {
		return digest.Digest{}, err
	}
	defer os.RemoveAll(tmp)
	var a Artifact
	for i, rel := range paths {
		src := filepath.Join(dir, filepath.FromSlash(rel))
		fi, err := os.Stat(src)
		if err != nil {
			return digest.Digest{}, err
		}
		idx, _, err := desync.IndexFromFile(ctx, src, e.Workers, MinChunk, AvgChunk, MaxChunk, desync.NullProgressBar{})
		if err != nil {
			return digest.Digest{}, fmt.Errorf("index %s: %w", rel, err)
		}
		if err := desync.ChopFile(ctx, src, idx.Chunks, e.Store, e.Workers, desync.NullProgressBar{}); err != nil {
			return digest.Digest{}, fmt.Errorf("chop %s: %w", rel, err)
		}
		name := fmt.Sprintf("%d.caibx", i)
		f, err := os.Create(filepath.Join(tmp, name))
		if err != nil {
			return digest.Digest{}, err
		}
		if _, err := idx.WriteTo(f); err != nil {
			f.Close()
			return digest.Digest{}, err
		}
		if err := f.Close(); err != nil {
			return digest.Digest{}, err
		}
		a.Files = append(a.Files, FileEntry{Path: rel, Size: fi.Size(), Mode: uint32(fi.Mode().Perm()), Index: name})
	}
	data, _ := json.Marshal(a)
	sum := sha256.Sum256(data)
	d, err := digest.Parse("sha256:" + hex.EncodeToString(sum[:]))
	if err != nil {
		return digest.Digest{}, err
	}
	if err := os.WriteFile(filepath.Join(tmp, "artifact.json"), data, 0o644); err != nil {
		return digest.Digest{}, err
	}
	final := filepath.Join(e.IndexDir, d.Hex())
	if _, err := os.Stat(final); err == nil {
		return d, nil // published before: same content
	}
	if err := os.Rename(tmp, final); err != nil {
		return digest.Digest{}, err
	}
	return d, nil
}

// Load returns the artifact's file list and the casync index of each file.
func (e *Engine) Load(d digest.Digest) (*Artifact, []desync.Index, error) {
	dir := filepath.Join(e.IndexDir, d.Hex())
	data, err := os.ReadFile(filepath.Join(dir, "artifact.json"))
	if err != nil {
		return nil, nil, err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != d.Hex() {
		return nil, nil, fmt.Errorf("artifact %s: artifact.json does not match its digest", d)
	}
	var a Artifact
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, nil, err
	}
	idxs := make([]desync.Index, len(a.Files))
	for i, f := range a.Files {
		r, err := os.Open(filepath.Join(dir, f.Index))
		if err != nil {
			return nil, nil, err
		}
		idxs[i], err = desync.IndexFromReader(r)
		r.Close()
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", f.Path, err)
		}
	}
	return &a, idxs, nil
}

// sameOffset marks the target chunks that the base file holds at the same offset. Both chunk
// lists are sorted by offset, so one pass suffices; no file is read.
func sameOffset(base, target []desync.IndexChunk) ([]bool, int64) {
	kept := make([]bool, len(target))
	var same int64
	j := 0
	for i, c := range target {
		for j < len(base) && base[j].Start < c.Start {
			j++
		}
		if j < len(base) && base[j].Start == c.Start && base[j].ID == c.ID && base[j].Size == c.Size {
			kept[i] = true
			same += int64(c.Size)
		}
	}
	return kept, same
}

// keepPlan pairs the target's files with a base's files by path.
type keepPlan struct {
	base  store.Base
	files *Artifact
	idxs  []desync.Index
	pair  []int    // base file per target file, -1: none
	kept  [][]bool // per target file, chunks the paired base file holds at the same offset
	same  int64
}

func (e *Engine) plan(target *Artifact, tidx []desync.Index, b store.Base) (keepPlan, error) {
	ba, bidx, err := e.Load(b.Manifest)
	if err != nil {
		return keepPlan{}, err
	}
	byPath := map[string]int{}
	for j, f := range ba.Files {
		byPath[f.Path] = j
	}
	p := keepPlan{base: b, files: ba, idxs: bidx, pair: make([]int, len(target.Files)), kept: make([][]bool, len(target.Files))}
	for i, f := range target.Files {
		j, ok := byPath[f.Path]
		if !ok {
			p.pair[i] = -1
			continue
		}
		p.pair[i] = j
		var s int64
		p.kept[i], s = sameOffset(bidx[j].Chunks, tidx[i].Chunks)
		p.same += s
	}
	return p, nil
}

// attestation is what a file looked like when this engine finished writing it.
type attestation struct {
	Ino     uint64 `json:"ino"`
	Size    int64  `json:"size"`
	MtimeNs int64  `json:"mtime_ns"`
}

func stat(p string) (attestation, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return attestation{}, err
	}
	a := attestation{Size: fi.Size(), MtimeNs: fi.ModTime().UnixNano()}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		a.Ino = uint64(st.Ino)
	}
	return a, nil
}

func (e *Engine) attestFile(d digest.Digest) string {
	return filepath.Join(e.AttestDir, d.Hex()+".json")
}

func (e *Engine) recordAttestation(d digest.Digest, dir string, a *Artifact) error {
	if e.AttestDir == "" {
		return nil
	}
	rec := map[string]attestation{}
	for _, f := range a.Files {
		s, err := stat(filepath.Join(dir, filepath.FromSlash(f.Path)))
		if err != nil {
			return err
		}
		rec[f.Path] = s
	}
	data, _ := json.Marshal(rec)
	tmp := e.attestFile(d) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, e.attestFile(d))
}

// Attest records the files of manifest d under dir as unchanged; call it only after Verify.
func (e *Engine) Attest(d digest.Digest, dir string) error {
	a, _, err := e.Load(d)
	if err != nil {
		return err
	}
	return e.recordAttestation(d, dir, a)
}

// unchanged reports whether the base file at p is the one this engine wrote for manifest d.
func (e *Engine) unchanged(d digest.Digest, path, p string) bool {
	if e.AttestDir == "" {
		return false
	}
	data, err := os.ReadFile(e.attestFile(d))
	if err != nil {
		return false
	}
	var rec map[string]attestation
	if json.Unmarshal(data, &rec) != nil {
		return false
	}
	want, ok := rec[path]
	if !ok {
		return false
	}
	got, err := stat(p)
	return err == nil && got == want
}

// Pull writes artifact m into the empty directory dest. With an offer it takes an idle aligned
// base over (Keep), or reads a base that stays intact (Copy); otherwise it assembles from the
// store (Reconstruct). It satisfies csi.Puller.
func (e *Engine) Pull(ctx context.Context, m digest.Digest, dest string, offer *csi.Offer) (store.PullStats, error) {
	start := time.Now()
	a, idxs, err := e.Load(m)
	if err != nil {
		return store.PullStats{}, err
	}
	st := store.PullStats{Files: len(a.Files)}
	for _, f := range a.Files {
		st.Bytes += f.Size
	}
	finals := make([]string, len(a.Files))
	for i, f := range a.Files {
		p := filepath.Join(dest, filepath.FromSlash(f.Path))
		if !filepath.IsLocal(filepath.FromSlash(f.Path)) {
			return st, fmt.Errorf("file path %q leaves the artifact", f.Path)
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return st, err
		}
		finals[i] = p
	}
	done := make([]bool, len(a.Files))
	minSame := e.MinSame
	if minSame <= 0 {
		minSame = store.DefaultMinSame
	}

	// Keep: the idle base that holds the most bytes at the same offsets
	if offer != nil && offer.Claim != nil && len(offer.Bases) > 0 {
		var best keepPlan
		for _, b := range offer.Bases {
			p, err := e.plan(a, idxs, b)
			if err == nil && p.same > best.same {
				best = p
			}
		}
		st.BaseBest = best.same
		if best.same > 0 && float64(best.same) >= minSame*float64(st.Bytes) {
			if dir, ok := offer.Claim(best.base); ok {
				st.Base = best.base.Manifest.String()
				if err := e.keep(ctx, a, idxs, best, dir, finals, done, offer.VerifyKept || e.VerifyKept, &st); err != nil {
					return st, err
				}
			}
		}
	}
	// Copy: a base that stays where it is seeds the files Keep did not produce
	var release func()
	var seedBase *keepPlan
	if offer != nil && len(offer.Readable) > 0 && offer.BaseCopy != store.BaseCopyNever {
		var most keepPlan
		for _, b := range offer.Readable {
			if p, err := e.plan(a, idxs, b); err == nil && p.same > most.same {
				most = p
			}
		}
		if most.same > 0 {
			ok := true
			release = func() {}
			if offer.Hold != nil {
				release, ok = offer.Hold(most.base)
			}
			if ok {
				seedBase = &most
				st.LocalBase = most.base.Manifest.String()
			}
		}
	}
	if release != nil {
		defer release()
	}
	// whatever is left: assemble from the store, seeded by the held base if there is one
	for i := range a.Files {
		if done[i] {
			continue
		}
		partial := finals[i] + ".casync-partial"
		os.Remove(partial)
		var seeds []desync.Seed
		if seedBase != nil && seedBase.pair[i] >= 0 {
			j := seedBase.pair[i]
			dir, _ := baseDir(offer, seedBase.base)
			src := filepath.Join(dir, filepath.FromSlash(seedBase.files.Files[j].Path))
			s, err := desync.NewIndexSeed(partial, src, seedBase.idxs[j])
			if err == nil {
				seeds = append(seeds, s)
				st.LocalBytes += countKept(seedBase.kept[i], idxs[i].Chunks)
			}
		}
		t0 := time.Now()
		if _, err := desync.AssembleFile(ctx, partial, idxs[i], e.Store, seeds, desync.AssembleOptions{N: e.Workers, InvalidSeedAction: desync.InvalidSeedActionSkip}); err != nil {
			os.Remove(partial)
			return st, fmt.Errorf("%s: %w", a.Files[i].Path, err)
		}
		e.logf("assemble %s (%d seeds): %s", a.Files[i].Path, len(seeds), time.Since(t0).Round(time.Millisecond))
		if err := e.finish(partial, finals[i], a.Files[i]); err != nil {
			return st, err
		}
		done[i] = true
	}
	if err := e.recordAttestation(m, dest, a); err != nil {
		return st, err
	}
	st.Duration = time.Since(start)
	return st, nil
}

// baseDir is where a readable base's files are: the materializer keeps them under its root, and
// the offer's Base.Dir says where.
func baseDir(_ *csi.Offer, b store.Base) (string, bool) { return b.Dir, b.Dir != "" }

func countKept(kept []bool, chunks []desync.IndexChunk) int64 {
	var n int64
	for i, k := range kept {
		if k {
			n += int64(chunks[i].Size)
		}
	}
	return n
}

// keep turns the claimed base's files into the target's: each paired file is moved into place,
// resized, and only its differing chunks are written from the store. Kept chunks are read back and
// hash-checked unless the file is attested unchanged since this engine wrote it.
func (e *Engine) keep(ctx context.Context, a *Artifact, idxs []desync.Index, p keepPlan, dir string, finals []string, done []bool, verify bool, st *store.PullStats) error {
	for i, j := range p.pair {
		if j < 0 {
			continue
		}
		src := filepath.Join(dir, filepath.FromSlash(p.files.Files[j].Path))
		trusted := !verify && e.unchanged(p.base.Manifest, p.files.Files[j].Path, src)
		partial := finals[i] + ".casync-partial"
		if err := os.Rename(src, partial); err != nil {
			continue // left for the store
		}
		f, err := os.OpenFile(partial, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		if err := f.Chmod(0o644); err != nil {
			f.Close()
			return err
		}
		if err := f.Truncate(a.Files[i].Size); err != nil {
			f.Close()
			return err
		}
		kept := append([]bool(nil), p.kept[i]...)
		t0 := time.Now()
		if !trusted {
			ok, bad := e.verifyKept(ctx, f, idxs[i].Chunks, kept)
			st.Verified += ok
			st.Repaired += bad
		}
		t1 := time.Now()
		written, err := e.writeChunks(ctx, f, idxs[i].Chunks, kept)
		if err != nil {
			f.Close()
			return err
		}
		t2 := time.Now()
		if e.Durable {
			if err := f.Sync(); err != nil {
				f.Close()
				return err
			}
		}
		if err := f.Close(); err != nil {
			return err
		}
		st.Reused += a.Files[i].Size - written
		st.ReadBytes += written
		e.logf("keep %s: trusted=%v verify %s, write %d B %s, sync %s", a.Files[i].Path, trusted,
			t1.Sub(t0).Round(time.Millisecond), written, t2.Sub(t1).Round(time.Millisecond), time.Since(t2).Round(time.Millisecond))
		if err := e.finish(partial, finals[i], a.Files[i]); err != nil {
			return err
		}
		done[i] = true
	}
	return nil
}

// verifyKept reads every kept chunk and checks its digest; a chunk that does not match is no
// longer kept, so the write step fetches it. It returns the bytes that matched and that did not.
func (e *Engine) verifyKept(ctx context.Context, f *os.File, chunks []desync.IndexChunk, kept []bool) (ok, bad int64) {
	var okN, badN atomic.Int64
	work := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < e.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var buf []byte
			for i := range work {
				c := chunks[i]
				if cap(buf) < int(c.Size) {
					buf = make([]byte, c.Size)
				}
				b := buf[:c.Size]
				if _, err := f.ReadAt(b, int64(c.Start)); err != nil || desync.ChunkID(desync.Digest.Sum(b)) != c.ID {
					kept[i] = false
					badN.Add(int64(c.Size))
					continue
				}
				okN.Add(int64(c.Size))
			}
		}()
	}
	for i := range chunks {
		if kept[i] {
			select {
			case work <- i:
			case <-ctx.Done():
			}
		}
	}
	close(work)
	wg.Wait()
	return okN.Load(), badN.Load()
}

// writeChunks writes every chunk that is not kept, from the store (each chunk's digest is checked
// when it is read from the store). It returns the bytes written.
func (e *Engine) writeChunks(ctx context.Context, f *os.File, chunks []desync.IndexChunk, kept []bool) (int64, error) {
	var written atomic.Int64
	var firstErr error
	var once sync.Once
	work := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < e.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				c := chunks[i]
				ch, err := e.Store.GetChunk(c.ID)
				if err == nil {
					var b []byte
					if b, err = ch.Data(); err == nil {
						if uint64(len(b)) != c.Size {
							err = fmt.Errorf("chunk %s: %d bytes, index says %d", c.ID, len(b), c.Size)
						} else if _, err = f.WriteAt(b, int64(c.Start)); err == nil {
							written.Add(int64(len(b)))
						}
					}
				}
				if err != nil {
					once.Do(func() { firstErr = err })
				}
			}
		}()
	}
	for i := range chunks {
		if !kept[i] {
			select {
			case work <- i:
			case <-ctx.Done():
				once.Do(func() { firstErr = ctx.Err() })
			}
		}
	}
	close(work)
	wg.Wait()
	return written.Load(), firstErr
}

// finish syncs a partial file (when the writer did not), sets its mode, and renames it into place.
func (e *Engine) finish(partial, final string, f FileEntry) error {
	if e.Durable {
		h, err := os.OpenFile(partial, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		if err := h.Sync(); err != nil {
			h.Close()
			return err
		}
		h.Close()
	}
	if err := os.Chmod(partial, os.FileMode(f.Mode)|0o400); err != nil {
		return err
	}
	return os.Rename(partial, final)
}

// Verify hash-checks every chunk of the artifact's files under dir against its indexes.
func (e *Engine) Verify(ctx context.Context, m digest.Digest, dir string) error {
	a, idxs, err := e.Load(m)
	if err != nil {
		return err
	}
	for i, f := range a.Files {
		h, err := os.Open(filepath.Join(dir, filepath.FromSlash(f.Path)))
		if err != nil {
			return err
		}
		kept := make([]bool, len(idxs[i].Chunks))
		for k := range kept {
			kept[k] = true
		}
		_, bad := e.verifyKept(ctx, h, idxs[i].Chunks, kept)
		fi, _ := h.Stat()
		h.Close()
		if bad > 0 || fi.Size() != f.Size {
			return fmt.Errorf("%s: %d bytes do not match the index", f.Path, bad)
		}
	}
	return nil
}

