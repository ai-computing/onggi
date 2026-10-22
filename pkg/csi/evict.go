package csi

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ai-computing/onggi/pkg/digest"
)

// Node-side eviction (#33). A materialized artifact is evicted when no volume uses it and nothing
// has used it for a while; the node cache is then collected with the artifacts still
// materialized as roots (cmd/onggi: the plugin's GC loop).

// usedFile records the last use of artifact d: its mtime is the time.
func (m *Materializer) usedFile(d digest.Digest) string {
	return filepath.Join(m.Root, "artifacts", d.Hex()+".used")
}

// touch notes that artifact d is in use now. The caller holds m.mu.
func (m *Materializer) touch(d digest.Digest) {
	p := m.usedFile(d)
	now := time.Now()
	if err := os.Chtimes(p, now, now); err != nil {
		os.WriteFile(p, nil, 0o644)
	}
}

func (m *Materializer) lastUsed(d digest.Digest) time.Time {
	if fi, err := os.Stat(m.usedFile(d)); err == nil {
		return fi.ModTime()
	}
	if fi, err := os.Stat(m.Dir(d)); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}

// Materialized lists the artifacts materialized on this node.
func (m *Materializer) Materialized() ([]digest.Digest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.materialized()
}

func (m *Materializer) materialized() ([]digest.Digest, error) {
	ents, err := os.ReadDir(filepath.Join(m.Root, "artifacts"))
	if err != nil {
		return nil, err
	}
	var out []digest.Digest
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if d, err := digest.Parse("sha256:" + e.Name()); err == nil {
			out = append(out, d)
		}
	}
	return out, nil
}

// Evict removes the artifacts that no volume uses and that nothing used for at least idle.
// unmount is called on the directory first: on Linux it holds the read-only self-bind that
// publishing made. The directory leaves artifacts/ by a rename under the materializer's lock,
// so Ensure either finds it complete or pulls it again; deleting the files happens after.
func (m *Materializer) Evict(now time.Time, idle time.Duration, unmount func(dir string) error) ([]digest.Digest, error) {
	m.mu.Lock()
	ds, err := m.materialized()
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	var gone []digest.Digest
	var trash []string
	for _, d := range ds {
		if _, busy := m.inflight[d]; busy {
			continue
		}
		if n, err := m.users(d); err != nil || n > 0 {
			continue
		}
		if now.Sub(m.lastUsed(d)) < idle {
			continue
		}
		dir := m.Dir(d)
		if unmount != nil {
			if err := unmount(dir); err != nil {
				m.logf("evict %s: %v", d, err)
				continue
			}
		}
		t, err := os.MkdirTemp(filepath.Join(m.Root, "tmp"), "evict-")
		if err != nil {
			m.mu.Unlock()
			return gone, err
		}
		dst := filepath.Join(t, d.Hex())
		if err := os.Rename(dir, dst); err != nil {
			os.Remove(t)
			m.logf("evict %s: %v", d, err)
			continue
		}
		os.Remove(m.usedFile(d))
		os.Remove(filepath.Join(m.Root, "leases", d.Hex()))
		trash = append(trash, t)
		gone = append(gone, d)
	}
	m.mu.Unlock()
	for _, t := range trash {
		if err := os.RemoveAll(t); err != nil && !strings.Contains(err.Error(), "no such file") {
			m.logf("evict: removing %s: %v", t, err)
		}
	}
	for _, d := range gone {
		m.logf("evicted %s (no volume, idle %s or more)", d, idle)
	}
	return gone, nil
}
