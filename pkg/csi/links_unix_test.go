//go:build unix

package csi

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A base whose file has a second hard link is not taken over: changing it in place would change
// the other name too. The pull then builds the target without the base, and both stay intact.
func TestOwnerHardLinkedDonorRefusesConsumption(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	m, c := newMaterializer(t, f)
	m.RebaseIdle = time.Hour
	if _, err := m.Ensure(ctx, f.a); err != nil {
		t.Fatal(err)
	}
	ageOwnerDonor(t, m, f.a)
	inside := filepath.Join(m.Dir(f.a), "model.bin")
	want, err := os.ReadFile(inside)
	if err != nil {
		t.Fatal(err)
	}
	// The second name lives under the materializer root so that both are on one filesystem.
	outside := filepath.Join(m.Root, "snapshot-model.bin")
	if err := os.Link(inside, outside); err != nil {
		t.Skipf("hard links unavailable here: %v", err)
	}
	if p, err := aliased(m.Dir(f.a)); err != nil || p != inside {
		t.Fatalf("aliased = %q, %v; want %q", p, err, inside)
	}

	offer, claimed := m.offer(f.b)
	if offer == nil || len(offer.Bases) != 1 || offer.Bases[0].Manifest != f.a {
		t.Fatal("expected the idle A donor to be offered")
	}
	if dir, ok := offer.Claim(offer.Bases[0]); ok || dir != "" || len(*claimed) != 0 {
		t.Fatalf("a hard-linked donor was consumed: dir=%q ok=%v", dir, ok)
	}

	dir, err := m.Ensure(ctx, f.b)
	if err != nil {
		t.Fatal(err)
	}
	sameTree(t, f.srcB, dir)
	sameTree(t, f.srcA, m.Dir(f.a))
	if got, err := os.ReadFile(outside); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("the second link changed: err=%v", err)
	}
	if last := c.pulls[len(c.pulls)-1]; last.Base != "" {
		t.Fatalf("the pull took over a hard-linked base: %+v", last)
	}

	// Without the second link the same donor is consumed as before.
	if err := os.Remove(outside); err != nil {
		t.Fatal(err)
	}
	if p, err := aliased(m.Dir(f.a)); err != nil || p != "" {
		t.Fatalf("aliased after unlink = %q, %v", p, err)
	}
}
