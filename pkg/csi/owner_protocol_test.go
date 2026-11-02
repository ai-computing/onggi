package csi

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ai-computing/onggi/pkg/digest"
	"github.com/ai-computing/onggi/pkg/store"
)

func ageOwnerDonor(t *testing.T, m *Materializer, d digest.Digest) {
	t.Helper()
	old := time.Unix(1, 0)
	if err := os.Chtimes(m.usedFile(d), old, old); err != nil {
		t.Fatal(err)
	}
}

// A stale offer cannot override a lease acquired before its actual claim.
func TestOwnerLeaseAfterOfferRefusesConsumption(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	m, c := newMaterializer(t, f)
	m.RebaseIdle = time.Hour
	if _, err := m.Ensure(ctx, f.a); err != nil {
		t.Fatal(err)
	}
	ageOwnerDonor(t, m, f.a)
	before, err := os.Stat(filepath.Join(m.Dir(f.a), "model.bin"))
	if err != nil {
		t.Fatal(err)
	}
	offer, claimed := m.offer(f.b)
	if offer == nil || len(offer.Bases) != 1 || offer.Bases[0].Manifest != f.a {
		t.Fatal("expected the idle A donor to be offered")
	}
	if err := m.Acquire("late-owner-lease", f.a, "/late-owner-target"); err != nil {
		t.Fatal(err)
	}
	// Keep age eligible so refusal is due to the lease rather than recency.
	ageOwnerDonor(t, m, f.a)
	if dir, ok := offer.Claim(offer.Bases[0]); ok || dir != "" || len(*claimed) != 0 {
		t.Fatalf("stale offer consumed a leased donor: dir=%q ok=%v", dir, ok)
	}
	if users, err := m.users(f.a); err != nil || users != 1 {
		t.Fatalf("lease lost: users=%d err=%v", users, err)
	}
	dir, err := m.Ensure(ctx, f.b)
	if err != nil {
		t.Fatal(err)
	}
	sameTree(t, f.srcB, dir)
	sameTree(t, f.srcA, m.Dir(f.a))
	after, err := os.Stat(filepath.Join(m.Dir(f.a), "model.bin"))
	if err != nil || !os.SameFile(before, after) || c.pulls[1].Base != "" {
		t.Fatalf("leased donor changed or was consumed: err=%v stats=%+v", err, c.pulls[1])
	}
	t.Log("offer preceded lease; saved claim refused; lease/inode/base bytes retained; separate target verified")
}

// Two distinct target offers contend on the real owner claim, not a stub callback.
func TestOwnerCompetingDonorClaims(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	m, _ := newMaterializer(t, f)
	m.RebaseIdle = time.Hour
	m.MaxPulls = 2
	if _, err := m.Ensure(ctx, f.a); err != nil {
		t.Fatal(err)
	}
	ageOwnerDonor(t, m, f.a)
	otherSource := t.TempDir()
	shared, err := os.ReadFile(filepath.Join(f.srcA, "model.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(otherSource, "model.bin"), append(shared, random(3, 100_000)...), 0o644); err != nil {
		t.Fatal(err)
	}
	other, _, err := f.s.Push(ctx, otherSource, store.PushOptions{})
	if err != nil {
		t.Fatal(err)
	}
	first, claimedFirst := m.offer(f.b)
	second, claimedSecond := m.offer(other)
	for _, offer := range []*Offer{first, second} {
		if offer == nil || len(offer.Bases) != 1 || offer.Bases[0].Manifest != f.a {
			t.Fatal("both distinct target offers must name A")
		}
	}
	before, err := os.Stat(filepath.Join(m.Dir(f.a), "model.bin"))
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		dir string
		ok  bool
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for _, offer := range []*Offer{first, second} {
		go func(o *Offer) {
			<-start
			dir, ok := o.Claim(o.Bases[0])
			results <- result{dir: dir, ok: ok}
		}(offer)
	}
	close(start)
	winner := ""
	wins := 0
	for range 2 {
		select {
		case r := <-results:
			if r.ok {
				wins++
				winner = r.dir
			} else if r.dir != "" {
				t.Fatalf("refused claim returned a directory: %q", r.dir)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("competing claims did not return")
		}
	}
	if wins != 1 || len(*claimedFirst)+len(*claimedSecond) != 1 || isDir(m.Dir(f.a)) {
		t.Fatalf("donor ownership moved incorrectly: wins=%d first=%v second=%v", wins, *claimedFirst, *claimedSecond)
	}
	sameTree(t, f.srcA, winner)
	after, err := os.Stat(filepath.Join(winner, "model.bin"))
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("winner did not receive the original donor inode: %v", err)
	}
	for _, offer := range []*Offer{first, second} {
		if _, ok := offer.Claim(offer.Bases[0]); ok {
			t.Fatal("a later claim took the already-transferred donor")
		}
	}
	sameTree(t, f.srcA, winner)
	t.Log("two real claims released together; exactly one move; winner bytes/inode intact after repeated refusal")
}

func ownerPackPaths(t *testing.T, root string) map[string]bool {
	t.Helper()
	paths := make(map[string]bool)
	err := filepath.WalkDir(filepath.Join(root, "packs"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			paths[path] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

// Actual residual corruption follows a successful real claim; retry has no donor.
func TestOwnerPostConsumptionFailureThenRetry(t *testing.T) {
	ctx := context.Background()
	storeDir := t.TempDir()
	f := &fixture{s: openStore(t, storeDir), srcA: t.TempDir(), srcB: t.TempDir()}
	shared := random(71, 3<<20)
	if err := os.WriteFile(filepath.Join(f.srcA, "model.bin"), shared, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.srcB, "model.bin"), append(shared, random(72, 100_000)...), 0o644); err != nil {
		t.Fatal(err)
	}
	var err error
	f.a, _, err = f.s.Push(ctx, f.srcA, store.PushOptions{})
	if err != nil {
		t.Fatal(err)
	}
	basePacks := ownerPackPaths(t, storeDir)
	f.b, _, err = f.s.Push(ctx, f.srcB, store.PushOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var residual []string
	for path := range ownerPackPaths(t, storeDir) {
		if !basePacks[path] {
			residual = append(residual, path)
		}
	}
	if len(residual) != 1 {
		t.Fatalf("expected one target-only residual pack, got %d", len(residual))
	}
	pack := residual[0]
	original, err := os.ReadFile(pack)
	if err != nil || len(original) == 0 {
		t.Fatalf("read residual pack: %v", err)
	}
	m, _ := newMaterializer(t, f)
	m.RebaseIdle = time.Hour
	if _, err := m.Ensure(ctx, f.a); err != nil {
		t.Fatal(err)
	}
	ageOwnerDonor(t, m, f.a)
	// No cache/peer can mask the corrupted target-only store bytes.
	realPull := StorePuller(f.s, nil, filepath.Join(m.Root, "tmp"), 2)
	claimed := false
	var injectionErr, pullErr error
	var failedStats store.PullStats
	m.Pull = func(ctx context.Context, target digest.Digest, dest string, offer *Offer) (store.PullStats, error) {
		if offer != nil {
			realClaim := offer.Claim
			offer.Claim = func(base store.Base) (string, bool) {
				dir, ok := realClaim(base)
				if ok {
					claimed = true
					corrupt := append([]byte(nil), original...)
					corrupt[len(corrupt)/2] ^= 0xff
					injectionErr = os.Chmod(pack, 0o644)
					if injectionErr == nil {
						injectionErr = os.WriteFile(pack, corrupt, 0o644)
					}
				}
				return dir, ok
			}
		}
		failedStats, pullErr = realPull(ctx, target, dest, offer)
		return failedStats, pullErr
	}
	_, err = m.Ensure(ctx, f.b)
	if !claimed || injectionErr != nil || pullErr == nil || !errors.Is(err, pullErr) {
		t.Fatalf("expected actual post-claim construction failure: claimed=%v injection=%v pull=%v ensure=%v", claimed, injectionErr, pullErr, err)
	}
	if failedStats.Base != f.a.String() || failedStats.Reused == 0 {
		t.Fatalf("failure preceded genuine consumption/preservation: %+v", failedStats)
	}
	if isDir(m.Dir(f.a)) || isDir(m.Dir(f.b)) {
		t.Fatal("consumed donor or incomplete target remained published")
	}
	if entries, err := os.ReadDir(filepath.Join(m.Root, "tmp")); err != nil || len(entries) != 0 {
		t.Fatalf("private/consumed cleanup failed: entries=%v err=%v", entries, err)
	}
	if err := os.WriteFile(pack, original, 0o644); err != nil {
		t.Fatal(err)
	}
	m.Pull = realPull
	dir, err := m.Ensure(ctx, f.b)
	if err != nil {
		t.Fatal(err)
	}
	sameTree(t, f.srcB, dir)
	if isDir(m.Dir(f.a)) {
		t.Fatal("retry manufactured a replacement donor")
	}
	t.Logf("real consumed-base residual failure=%v reused=%d; no completed target; donor-free retry bytes verified", pullErr, failedStats.Reused)
}
