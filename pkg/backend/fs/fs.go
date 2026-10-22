// Package fs is the POSIX backend: local disk, NFS, SMB, CephFS, JuiceFS, Lustre — anything mounted.
//
// Layout under the root: <kind>/<first two hex>/<name> for objects, refs/<name> for refs, tmp/ for
// writes in progress. An object becomes visible only by a link or rename of a complete, synced
// temporary file, so a crash leaves at most a stray file in tmp/.
package fs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ai-computing/onggi/pkg/backend"
)

// Store is an fs backend rooted at a directory.
type Store struct {
	root       string
	durability backend.Durability
}

// Open returns a store at root, creating it if needed. durability is what the mount offers
// (onggi cannot see it from here); pass backend.DurabilityUnknown when not known.
func Open(root string, durability backend.Durability) (*Store, error) {
	for _, d := range []string{"tmp", "refs"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return nil, err
		}
	}
	if durability == "" {
		durability = backend.DurabilityUnknown
	}
	return &Store{root: root, durability: durability}, nil
}

func (s *Store) path(k backend.Key) (string, error) {
	if k.Name == "" || strings.ContainsAny(k.Name, `/\`) || k.Name == "." || k.Name == ".." || strings.ContainsAny(string(k.Kind), `/\.`) {
		return "", fmt.Errorf("bad key %q", k)
	}
	shard := k.Name
	if len(shard) > 2 {
		shard = shard[:2]
	}
	return filepath.Join(s.root, string(k.Kind), shard, k.Name), nil
}

func (s *Store) tmp() (*os.File, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Join(s.root, "tmp", hex.EncodeToString(b[:])), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
}

// Put writes to a temporary file, syncs it, then publishes it with a hard link, which fails if
// the key exists. Mounts without hard links (some SMB servers) fall back to rename; for content
// keys a racing rename replaces identical bytes, which is harmless.
func (s *Store) Put(ctx context.Context, k backend.Key, r io.Reader, size int64) error {
	dst, err := s.path(k)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dst); err == nil {
		_, _ = io.Copy(io.Discard, r)
		return nil
	}
	f, err := s.tmp()
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	n, err := io.Copy(f, r)
	if err == nil && n != size {
		err = fmt.Errorf("put %s: got %d bytes, want %d", k, n, size)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Link(tmp, dst); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil
		}
		if rerr := os.Rename(tmp, dst); rerr != nil {
			return fmt.Errorf("publish %s: link: %v; rename: %v", k, err, rerr)
		}
	}
	return nil
}

// Get opens the object and reads a range of it.
func (s *Store) Get(ctx context.Context, k backend.Key, off, n int64) (io.ReadCloser, error) {
	p, err := s.path(k)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", k, backend.ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	if n < 0 {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			f.Close()
			return nil, err
		}
		return f, nil
	}
	return struct {
		io.Reader
		io.Closer
	}{io.NewSectionReader(f, off, n), f}, nil
}

// Has stats each key.
func (s *Store) Has(ctx context.Context, keys []backend.Key) ([]bool, error) {
	out := make([]bool, len(keys))
	for i, k := range keys {
		p, err := s.path(k)
		if err != nil {
			return nil, err
		}
		_, err = os.Stat(p)
		out[i] = err == nil
	}
	return out, nil
}

// List walks the kind's directory.
func (s *Store) List(ctx context.Context, kind backend.Kind, fn func(string, int64) error) error {
	dir := filepath.Join(s.root, string(kind))
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return fn(d.Name(), info.Size())
	})
}

// Delete removes an object.
func (s *Store) Delete(ctx context.Context, k backend.Key) error {
	p, err := s.path(k)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Caps: range reads; atomic create through link or rename.
func (s *Store) Caps() backend.Capabilities {
	return backend.Capabilities{RangeReads: true, AtomicCreate: true, Durability: s.durability}
}

// ---- refs: a lock file per ref makes SetRef a compare-and-swap on a single mount.

const lockStale = 2 * time.Minute

func (s *Store) refPath(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, `\`) || strings.Contains(name, "..") || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("bad ref name %q", name)
	}
	return filepath.Join(s.root, "refs", filepath.FromSlash(escape(name))), nil
}

// escape keeps ref names like "llama-3.1-8b@atom/decode-s512-b8" as one file name.
func escape(name string) string { return strings.ReplaceAll(name, "/", "%2F") }

func unescape(name string) string { return strings.ReplaceAll(name, "%2F", "/") }

func (s *Store) GetRef(ctx context.Context, name string) (string, error) {
	p, err := s.refPath(name)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("ref %s: %w", name, backend.ErrNotFound)
	}
	return strings.TrimSpace(string(b)), err
}

func (s *Store) SetRef(ctx context.Context, name, old, value string) error {
	p, err := s.refPath(name)
	if err != nil {
		return err
	}
	lock := p + ".lock"
	for {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			f.Close()
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
		if info, serr := os.Stat(lock); serr == nil && time.Since(info.ModTime()) > lockStale {
			os.Remove(lock) // a writer died holding it
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	defer os.Remove(lock)
	cur, err := s.GetRef(ctx, name)
	if errors.Is(err, backend.ErrNotFound) {
		cur, err = "", nil
	}
	if err != nil {
		return err
	}
	if cur != old {
		return fmt.Errorf("ref %s is %q, expected %q: %w", name, cur, old, backend.ErrConflict)
	}
	if value == "" {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	f, err := s.tmp()
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.WriteString(value + "\n")
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, p)
}

func (s *Store) ListRefs(ctx context.Context, fn func(name, value string) error) error {
	ents, err := os.ReadDir(filepath.Join(s.root, "refs"))
	if err != nil {
		return err
	}
	for _, e := range ents {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".lock") {
			continue
		}
		name := unescape(e.Name())
		v, err := s.GetRef(ctx, name)
		if err != nil {
			return err
		}
		if err := fn(name, v); err != nil {
			return err
		}
	}
	return nil
}
