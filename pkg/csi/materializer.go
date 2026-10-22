// Package csi is onggi's CSI node plugin (ADR-0004): it serves ephemeral inline volumes whose
// files are an artifact, materialized once per node and bind-mounted read-only into each pod.
package csi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/ai-computing/onggi/pkg/digest"
	"github.com/ai-computing/onggi/pkg/store"
)

// Puller writes the artifact with manifest m into the empty directory dest. offer, if not nil,
// names artifacts on the node the pull may turn into m (delta apply, #41).
type Puller func(ctx context.Context, m digest.Digest, dest string, offer *Offer) (store.PullStats, error)

// Offer is what a materializer offers a pull for delta apply: idle artifacts, and Claim, which
// takes one away from the materializer and returns the directory its files are in now.
type Offer struct {
	Bases []store.Base
	Claim func(store.Base) (dir string, ok bool)
}

// StorePuller pulls from s through the node cache (nil: no cache), assembling cache packs in tmp.
func StorePuller(s *store.Store, cache *store.Store, tmp string, workers int) Puller {
	return StorePullerOpen(s, func(context.Context) (*store.Store, error) { return cache, nil }, tmp, workers)
}

// StorePullerOpen is StorePuller with the node cache opened afresh for each pull (nil: no
// cache), so a pull sees the cache as the plugin's GC loop left it (#33).
func StorePullerOpen(s *store.Store, openCache func(context.Context) (*store.Store, error), tmp string, workers int) Puller {
	return StorePullerPeers(s, openCache, nil, tmp, workers)
}

// StorePullerPeers is StorePullerOpen that also asks the peers returned by peers (nil: none)
// for chunks the node cache lacks, before the store (#37).
func StorePullerPeers(s *store.Store, openCache func(context.Context) (*store.Store, error),
	peers func(context.Context) []store.ChunkSource, tmp string, workers int) Puller {
	return func(ctx context.Context, m digest.Digest, dest string, offer *Offer) (store.PullStats, error) {
		cache, err := openCache(ctx)
		if err != nil {
			return store.PullStats{}, err
		}
		o := store.PullOptions{Workers: workers, Cache: cache, CacheTmp: tmp}
		if peers != nil {
			o.Peers = peers(ctx)
		}
		if offer != nil {
			o.Bases, o.Claim = offer.Bases, offer.Claim
		}
		return s.Pull(ctx, m, dest, o)
	}
}

// Materializer keeps one copy of each artifact on the node, under Root:
//
//	artifacts/<manifest hex>/   the artifact's files; a directory here is complete
//	tmp/                        pulls in progress, renamed into artifacts/ when done
//	volumes/<volume>.json       which artifact a published volume uses, and where
//	leases/<manifest hex>/<vol> one file per volume that uses the artifact
//
// The directory of an artifact is named by its manifest digest, so pods with the same ref share
// it, and a ref moved to a new manifest gets a new directory. artifacts/<hex>.used records the
// last use; Evict removes directories no lease holds and nothing used recently (#33).
type Materializer struct {
	Root string
	Pull Puller
	Logf func(format string, args ...any) // nil: no log
	// RebaseIdle enables delta apply (#41): a pull may consume an artifact no volume uses and
	// nothing used for this long, writing only what differs from it. 0: off.
	RebaseIdle time.Duration
	// Unmount removes the read-only self-bind publishing put on an artifact directory (nil: none).
	Unmount func(dir string) error

	mu       sync.Mutex
	inflight map[digest.Digest]*flight
}

type flight struct {
	done chan struct{}
	err  error
}

// Volume is what the materializer records about a published volume.
type Volume struct {
	Manifest string `json:"manifest"`
	Target   string `json:"target"`
}

func (m *Materializer) logf(format string, args ...any) {
	if m.Logf != nil {
		m.Logf(format, args...)
	}
}

// Dir is where artifact d is (or will be) materialized.
func (m *Materializer) Dir(d digest.Digest) string {
	return filepath.Join(m.Root, "artifacts", d.Hex())
}

// Init creates the layout and removes pulls that a previous process left unfinished. Only one
// materializer may use Root at a time.
func (m *Materializer) Init() error {
	for _, d := range []string{"artifacts", "tmp", "volumes", "leases"} {
		if err := os.MkdirAll(filepath.Join(m.Root, d), 0o755); err != nil {
			return err
		}
	}
	tmp := filepath.Join(m.Root, "tmp")
	ents, err := os.ReadDir(tmp)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if err := os.RemoveAll(filepath.Join(tmp, e.Name())); err != nil {
			return err
		}
		m.logf("removed unfinished pull %s", e.Name())
	}
	return nil
}

// Ensure returns the directory of artifact d, materializing it first unless it is there.
// Concurrent calls for one digest share one pull. The pull does not stop when ctx ends: Ensure
// then returns ctx's error and the pull goes on, so a retry (kubelet retries NodePublishVolume
// after its timeout) finds the directory or joins the pull.
func (m *Materializer) Ensure(ctx context.Context, d digest.Digest) (string, error) {
	dir := m.Dir(d)
	m.mu.Lock()
	if isDir(dir) {
		m.touch(d)
		m.mu.Unlock()
		return dir, nil
	}
	if m.inflight == nil {
		m.inflight = map[digest.Digest]*flight{}
	}
	f, ok := m.inflight[d]
	if !ok {
		f = &flight{done: make(chan struct{})}
		m.inflight[d] = f
		go func() {
			f.err = m.materialize(d)
			m.mu.Lock()
			delete(m.inflight, d)
			m.mu.Unlock()
			close(f.done)
		}()
	}
	m.mu.Unlock()
	select {
	case <-f.done:
		if f.err != nil {
			return "", f.err
		}
		m.mu.Lock()
		m.touch(d)
		m.mu.Unlock()
		return dir, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// materialize pulls d into a fresh directory under tmp/ and renames it into artifacts/, so a
// directory in artifacts/ is always complete.
func (m *Materializer) materialize(d digest.Digest) error {
	dir := m.Dir(d)
	tmp, err := os.MkdirTemp(filepath.Join(m.Root, "tmp"), d.Hex()[:16]+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp) // gone after a successful rename
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	m.logf("materializing %s", d)
	start := time.Now()
	offer, claimed := m.offer(d)
	st, err := m.Pull(context.Background(), d, tmp, offer)
	for _, c := range *claimed {
		os.RemoveAll(c) // what the pull did not move of a consumed base
	}
	switch {
	case st.Base != "":
		m.logf("pull of %s took over %s: %d B left in place", d, st.Base, st.Reused)
	case err == nil && offer != nil:
		m.logf("pull of %s took over nothing: of %d idle artifacts the best keeps %d of %d B at the same offsets (%.1f %%, %.0f %% needed)",
			d, len(offer.Bases), st.BaseBest, st.Bytes, 100*float64(st.BaseBest)/float64(max(st.Bytes, 1)), 100*store.DefaultMinSame)
	}
	if err != nil {
		m.logf("materializing %s failed after %s: %v", d, time.Since(start).Round(time.Millisecond), err)
		return fmt.Errorf("pull %s: %w", d, err)
	}
	if err := os.Rename(tmp, dir); err != nil {
		if isDir(dir) { // published by someone else meanwhile; same content
			return nil
		}
		return err
	}
	syncDir(filepath.Dir(dir))
	js, _ := json.Marshal(st)
	m.logf("materialized %s in %s: %s", d, time.Since(start).Round(time.Millisecond), js)
	return nil
}

// volumeName maps a CSI volume id to a file name. Kubelet's ids for ephemeral volumes
// ("csi-<hex>") are kept as they are; anything else is hashed.
func volumeName(id string) string {
	ok := id != "" && id != "." && id != ".." && len(id) <= 200
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			ok = false
			break
		}
	}
	if ok {
		return id
	}
	h := sha256.Sum256([]byte(id))
	return "h-" + hex.EncodeToString(h[:])
}

// Acquire records that volume id uses artifact d at target. It is idempotent.
func (m *Materializer) Acquire(id string, d digest.Digest, target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	name := volumeName(id)
	ldir := filepath.Join(m.Root, "leases", d.Hex())
	if err := os.MkdirAll(ldir, 0o755); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(ldir, name), []byte(target+"\n")); err != nil {
		return err
	}
	data, _ := json.Marshal(Volume{Manifest: d.String(), Target: target})
	return writeAtomic(filepath.Join(m.Root, "volumes", name+".json"), data)
}

// Release drops volume id's lease and returns the artifact it used and how many volumes still
// use it. A volume that holds no lease returns ok false. The artifact's directory stays.
func (m *Materializer) Release(id string) (d digest.Digest, users int, ok bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	name := volumeName(id)
	vf := filepath.Join(m.Root, "volumes", name+".json")
	data, err := os.ReadFile(vf)
	if errors.Is(err, os.ErrNotExist) {
		return d, 0, false, nil
	}
	if err != nil {
		return d, 0, false, err
	}
	var v Volume
	if err := json.Unmarshal(data, &v); err != nil {
		return d, 0, false, fmt.Errorf("volume %s: %v", id, err)
	}
	if d, err = digest.Parse(v.Manifest); err != nil {
		return d, 0, false, fmt.Errorf("volume %s: %v", id, err)
	}
	if err := os.Remove(filepath.Join(m.Root, "leases", d.Hex(), name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return d, 0, false, err
	}
	if err := os.Remove(vf); err != nil {
		return d, 0, false, err
	}
	m.touch(d)
	users, err = m.users(d)
	return d, users, true, err
}

// Users counts the volumes that use artifact d.
func (m *Materializer) Users(d digest.Digest) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.users(d)
}

func (m *Materializer) users(d digest.Digest) (int, error) {
	ents, err := os.ReadDir(filepath.Join(m.Root, "leases", d.Hex()))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	n := 0
	for _, e := range ents {
		if !e.IsDir() && filepath.Ext(e.Name()) != ".tmp" {
			n++
		}
	}
	return n, err
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func syncDir(p string) {
	if f, err := os.Open(p); err == nil {
		f.Sync()
		f.Close()
	}
}

// offer lists the artifacts a pull of d may consume (#41): no volume, no pull, and unused for
// RebaseIdle, most recently used first, at most three. Claim takes one out of artifacts/ under the
// lock, so Ensure and Evict no longer see it; the directories it hands out are listed in claimed.
func (m *Materializer) offer(d digest.Digest) (*Offer, *[]string) {
	claimed := new([]string)
	if m.RebaseIdle <= 0 {
		return nil, claimed
	}
	m.mu.Lock()
	ds, _ := m.materialized()
	var bases []store.Base
	inUse, recent := 0, 0
	for _, b := range ds {
		switch {
		case b == d:
		case m.idle(b, time.Now()):
			bases = append(bases, store.Base{Manifest: b, Dir: m.Dir(b)})
		case m.busy(b):
			inUse++
		default:
			recent++
		}
	}
	m.mu.Unlock()
	if len(bases) == 0 {
		if len(ds) > 0 {
			m.logf("pull of %s: no artifact to take over (%d in use, %d used within %s)", d, inUse, recent, m.RebaseIdle)
		}
		return nil, claimed
	}
	sort.Slice(bases, func(i, j int) bool { return m.lastUsed(bases[i].Manifest).After(m.lastUsed(bases[j].Manifest)) })
	if len(bases) > 3 {
		bases = bases[:3]
	}
	claim := func(b store.Base) (string, bool) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if !m.idle(b.Manifest, time.Now()) {
			return "", false
		}
		dir := m.Dir(b.Manifest)
		if m.Unmount != nil {
			if err := m.Unmount(dir); err != nil {
				m.logf("rebase from %s: %v", b.Manifest, err)
				return "", false
			}
		}
		t, err := os.MkdirTemp(filepath.Join(m.Root, "tmp"), "rebase-")
		if err != nil {
			return "", false
		}
		dst := filepath.Join(t, b.Manifest.Hex())
		if err := os.Rename(dir, dst); err != nil {
			os.Remove(t)
			m.logf("rebase from %s: %v", b.Manifest, err)
			return "", false
		}
		os.Remove(m.usedFile(b.Manifest))
		os.Remove(filepath.Join(m.Root, "leases", b.Manifest.Hex()))
		*claimed = append(*claimed, t)
		return dst, true
	}
	return &Offer{Bases: bases, Claim: claim}, claimed
}

// idle says whether artifact d may be consumed: materialized, no volume, no pull, unused for
// RebaseIdle. The caller holds m.mu.
func (m *Materializer) idle(d digest.Digest, now time.Time) bool {
	if m.busy(d) || !isDir(m.Dir(d)) {
		return false
	}
	return now.Sub(m.lastUsed(d)) >= m.RebaseIdle
}

// busy says whether a volume or a pull uses artifact d. The caller holds m.mu.
func (m *Materializer) busy(d digest.Digest) bool {
	if _, ok := m.inflight[d]; ok {
		return true
	}
	n, err := m.users(d)
	return err != nil || n > 0
}
