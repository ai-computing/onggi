package csi

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ai-computing/onggi/pkg/store"
)

// An artifact a volume uses is never offered for consumption, but a pull may read it where it is.
func TestLeasedArtifactIsReadNotConsumed(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	m, c := newMaterializer(t, f)
	m.RebaseIdle = time.Hour
	if _, err := m.Ensure(ctx, f.a); err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire("vol-a", f.a, "/target-a"); err != nil {
		t.Fatal(err)
	}
	ageOwnerDonor(t, m, f.a)
	before, err := os.Stat(filepath.Join(m.Dir(f.a), "model.bin"))
	if err != nil {
		t.Fatal(err)
	}
	offer, _ := m.offer(f.b)
	if offer == nil || len(offer.Bases) != 0 || len(offer.Readable) != 1 || offer.Readable[0].Manifest != f.a {
		t.Fatalf("offer %+v: the leased artifact must be readable and not consumable", offer)
	}
	dir, err := m.Ensure(ctx, f.b)
	if err != nil {
		t.Fatal(err)
	}
	sameTree(t, f.srcB, dir)
	sameTree(t, f.srcA, m.Dir(f.a))
	last := c.pulls[len(c.pulls)-1]
	if last.Base != "" || last.LocalBase != f.a.String() || last.LocalBytes == 0 {
		t.Fatalf("the pull did not read the leased artifact where it is: %+v", last)
	}
	after, err := os.Stat(filepath.Join(m.Dir(f.a), "model.bin"))
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("the leased artifact changed: %v", err)
	}
	if n, err := m.users(f.a); err != nil || n != 1 {
		t.Fatalf("lease lost: %d, %v", n, err)
	}
}

// While a pull reads an artifact, another pull cannot take it over and eviction leaves it alone.
func TestReadHoldBlocksClaimAndEviction(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	m, _ := newMaterializer(t, f)
	m.RebaseIdle = time.Hour
	if _, err := m.Ensure(ctx, f.a); err != nil {
		t.Fatal(err)
	}
	ageOwnerDonor(t, m, f.a)
	offer, claimed := m.offer(f.b)
	if offer == nil || len(offer.Bases) != 1 || offer.Hold == nil {
		t.Fatalf("offer %+v", offer)
	}
	release, ok := offer.Hold(offer.Readable[0])
	if !ok {
		t.Fatal("could not hold a published artifact")
	}
	if dir, ok := offer.Claim(offer.Bases[0]); ok || dir != "" || len(*claimed) != 0 {
		t.Fatal("an artifact that a pull is reading was taken over")
	}
	if gone, err := m.Evict(time.Now(), 0, nil); err != nil || len(gone) != 0 {
		t.Fatalf("an artifact that a pull is reading was evicted: %v, %v", gone, err)
	}
	release()
	ageOwnerDonor(t, m, f.a)
	if dir, ok := offer.Claim(offer.Bases[0]); !ok || dir == "" {
		t.Fatal("the artifact could not be taken over after the read ended")
	}
}

// With a limit, the cache policy picks the victim: only the least recently used idle artifact is
// offered for consumption, whatever the others would keep in place. All of them may be read.
func TestOnlyTheEvictionVictimIsOffered(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	m, _ := newMaterializer(t, f)
	m.RebaseIdle = time.Hour
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "model.bin"), random(7, 2<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	o, _, err := f.s.Push(ctx, other, store.PushOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []struct {
		name string
		age  time.Duration
	}{{"a", 3 * time.Hour}, {"o", 2 * time.Hour}} {
		x := f.a
		if d.name == "o" {
			x = o
		}
		if _, err := m.Ensure(ctx, x); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-d.age)
		if err := os.Chtimes(m.usedFile(x), when, when); err != nil {
			t.Fatal(err)
		}
	}
	offer, _ := m.offer(f.b)
	if offer == nil || len(offer.Bases) != 2 || len(offer.Readable) != 2 {
		t.Fatalf("without a limit both idle artifacts are offered: %+v", offer)
	}
	m.MaxArtifacts = 2
	offer, _ = m.offer(f.b)
	if offer == nil || len(offer.Bases) != 1 || offer.Bases[0].Manifest != f.a || len(offer.Readable) != 2 {
		t.Fatalf("with a limit only the least recently used idle artifact is offered: %+v", offer)
	}
	// make the other one the least recently used: it becomes the victim, though it shares nothing with B
	when := time.Now().Add(-4 * time.Hour)
	if err := os.Chtimes(m.usedFile(o), when, when); err != nil {
		t.Fatal(err)
	}
	offer, _ = m.offer(f.b)
	if offer == nil || len(offer.Bases) != 1 || offer.Bases[0].Manifest != o {
		t.Fatalf("the victim is the least recently used artifact, not the best aligned one: %+v", offer)
	}
}

// Leases are files: a new materializer on the same root (a restarted plugin) still refuses to
// take over an artifact a volume uses.
func TestLeasesSurviveARestart(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	m, _ := newMaterializer(t, f)
	if _, err := m.Ensure(ctx, f.a); err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire("vol-a", f.a, "/target-a"); err != nil {
		t.Fatal(err)
	}
	c := &counting{Puller: StorePuller(f.s, f.cache, filepath.Join(m.Root, "tmp"), 4)}
	again := &Materializer{Root: m.Root, Pull: c.pull, Logf: t.Logf, RebaseIdle: time.Hour}
	if err := again.Init(); err != nil {
		t.Fatal(err)
	}
	ageOwnerDonor(t, again, f.a)
	if n, err := again.Users(f.a); err != nil || n != 1 {
		t.Fatalf("the restarted materializer sees %d leases, %v", n, err)
	}
	if offer, _ := again.offer(f.b); offer == nil || len(offer.Bases) != 0 {
		t.Fatalf("a leased artifact was offered for consumption after a restart: %+v", offer)
	}
	dir, err := again.Ensure(ctx, f.b)
	if err != nil {
		t.Fatal(err)
	}
	sameTree(t, f.srcB, dir)
	sameTree(t, f.srcA, again.Dir(f.a))
	if _, _, ok, err := again.Release("vol-a"); err != nil || !ok {
		t.Fatalf("the lease could not be released after the restart: %v", err)
	}
	ageOwnerDonor(t, again, f.a)
	if offer, _ := again.offer(f.b); offer == nil || len(offer.Bases) != 1 {
		t.Fatalf("the released artifact is not offered: %+v", offer)
	}
}
