package csi

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ai-computing/onggi/pkg/digest"
	"github.com/ai-computing/onggi/pkg/store"
)

// Below MaxArtifacts a pull consumes nothing, however idle the other artifact is: the node has
// room for both. At the limit the idle artifact is taken over, and the node still holds as many
// artifacts as before.
func TestOwnerFreeSlotRefusesConsumption(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	m, c := newMaterializer(t, f)
	m.RebaseIdle = time.Hour
	m.MaxArtifacts = 2
	if _, err := m.Ensure(ctx, f.a); err != nil {
		t.Fatal(err)
	}
	ageOwnerDonor(t, m, f.a)
	dir, err := m.Ensure(ctx, f.b)
	if err != nil {
		t.Fatal(err)
	}
	sameTree(t, f.srcB, dir)
	sameTree(t, f.srcA, m.Dir(f.a))
	if last := c.pulls[len(c.pulls)-1]; last.Base != "" {
		t.Fatalf("a pull consumed an artifact although a slot was free: %+v", last)
	}
	if ds, err := m.Materialized(); err != nil || len(ds) != 2 {
		t.Fatalf("materialized %v, %v; want both", ds, err)
	}

	// The node is full now. Drop B and pull it again with the limit at one: A is consumed.
	if err := os.RemoveAll(m.Dir(f.b)); err != nil {
		t.Fatal(err)
	}
	m.MaxArtifacts = 1
	ageOwnerDonor(t, m, f.a)
	if dir, err = m.Ensure(ctx, f.b); err != nil {
		t.Fatal(err)
	}
	sameTree(t, f.srcB, dir)
	if last := c.pulls[len(c.pulls)-1]; last.Base != f.a.String() {
		t.Fatalf("a full node did not take the idle artifact over: %+v", last)
	}
	if ds, err := m.Materialized(); err != nil || len(ds) != 1 || ds[0] != f.b {
		t.Fatalf("materialized %v, %v; want only B", ds, err)
	}
}

// Evict brings a node back under MaxArtifacts by removing the least recently used unused
// artifact, and leaves an artifact a volume uses alone even if that keeps the node over.
func TestEvictAboveMaxArtifacts(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	m, _ := newMaterializer(t, f)
	if _, err := m.Ensure(ctx, f.a); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Ensure(ctx, f.b); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	older, newer := now.Add(-2*time.Minute), now.Add(-time.Minute)
	if err := os.Chtimes(m.usedFile(f.a), older, older); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(m.usedFile(f.b), newer, newer); err != nil {
		t.Fatal(err)
	}

	// no limit: nothing is idle for an hour, nothing goes
	if gone, err := m.Evict(now, time.Hour, nil); err != nil || len(gone) != 0 {
		t.Fatalf("evicted %v, %v without a limit", gone, err)
	}
	// limit one, but the older artifact is in use: the newer, unused one goes instead
	m.MaxArtifacts = 1
	if err := m.Acquire("vol-a", f.a, "/target-a"); err != nil {
		t.Fatal(err)
	}
	gone, err := m.Evict(now, time.Hour, nil)
	if err != nil || len(gone) != 1 || gone[0] != f.b {
		t.Fatalf("evicted %v, %v; want the unused B", gone, err)
	}
	if _, err := os.Stat(filepath.Join(m.Dir(f.a), "model.bin")); err != nil {
		t.Fatalf("the artifact in use was removed: %v", err)
	}

	// both unused: the least recently used goes
	if _, _, _, err := m.Release("vol-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Ensure(ctx, f.b); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(m.usedFile(f.a), older, older); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(m.usedFile(f.b), newer, newer); err != nil {
		t.Fatal(err)
	}
	gone, err = m.Evict(now, time.Hour, nil)
	if err != nil || len(gone) != 1 || gone[0] != f.a {
		t.Fatalf("evicted %v, %v; want the least recently used A", gone, err)
	}
}

// The materializer hands its policies to the pull.
func TestOfferCarriesPolicies(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	m, _ := newMaterializer(t, f)
	m.RebaseIdle = time.Hour
	m.VerifyKept, m.BaseCopy = true, store.BaseCopyRemote
	if _, err := m.Ensure(ctx, f.a); err != nil {
		t.Fatal(err)
	}
	ageOwnerDonor(t, m, f.a)
	offer, _ := m.offer(f.b)
	if offer == nil || !offer.VerifyKept || offer.BaseCopy != store.BaseCopyRemote {
		t.Fatalf("offer %+v does not carry the policies", offer)
	}
	var seen store.PullStats
	inner := m.Pull
	m.Pull = func(ctx context.Context, d digest.Digest, dest string, o *Offer) (store.PullStats, error) {
		st, err := inner(ctx, d, dest, o)
		seen = st
		return st, err
	}
	if _, err := m.Ensure(ctx, f.b); err != nil {
		t.Fatal(err)
	}
	if seen.Base != f.a.String() || seen.Verified <= 0 || seen.Verified != seen.Reused {
		t.Fatalf("the pull did not verify what it kept: %+v", seen)
	}
}
