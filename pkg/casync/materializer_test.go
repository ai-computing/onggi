package casync

import (
	"bytes"
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ai-computing/onggi/pkg/csi"
	"github.com/ai-computing/onggi/pkg/store"
)

// TestOwnership runs the materializer with the casync engine: a sibling a volume uses is never
// consumed (the new variant is built without destroying it), and once released and idle it is the
// one consumed, writing only what differs.
func TestOwnership(t *testing.T) {
	ctx := context.Background()
	e, root := setup(t)
	base, aligned, _ := variants()
	third := append([]byte(nil), aligned...)
	rand.New(rand.NewSource(9)).Read(third[1<<20 : 1<<20+200<<10])
	mA, _ := e.Publish(ctx, publish(t, e, root, "a", base))
	mB, _ := e.Publish(ctx, publish(t, e, root, "b", aligned))
	mC, _ := e.Publish(ctx, publish(t, e, root, "c", third))

	m := &csi.Materializer{Root: filepath.Join(root, "node"), Pull: e.Pull, RebaseIdle: time.Nanosecond,
		MaxArtifacts: 1, BaseCopy: store.BaseCopyAlways}
	if err := m.Init(); err != nil {
		t.Fatal(err)
	}
	dA, err := m.Ensure(ctx, mA)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire("vol-a", mA, "/pods/a"); err != nil {
		t.Fatal(err)
	}
	// A is in use: B must not consume it
	dB, err := m.Ensure(ctx, mB)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readFile(t, filepath.Join(dA, "model.bin")), base) {
		t.Fatal("a sibling in use was modified")
	}
	if !bytes.Equal(readFile(t, filepath.Join(dB, "model.bin")), aligned) {
		t.Fatal("B is wrong")
	}
	// Release marks A used now, so B is the least recently used idle sibling: C consumes B, not A
	if _, _, _, err := m.Release("vol-a"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	dC, err := m.Ensure(ctx, mC)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readFile(t, filepath.Join(dC, "model.bin")), third) {
		t.Fatal("C is wrong")
	}
	if _, err := os.Stat(dB); !os.IsNotExist(err) {
		t.Fatal("C did not consume B, the least recently used idle sibling")
	}
	if !bytes.Equal(readFile(t, filepath.Join(dA, "model.bin")), base) {
		t.Fatal("A changed although it was not the victim")
	}
	// next, the released A is the least recently used idle sibling and is the one consumed
	time.Sleep(20 * time.Millisecond)
	dB2, err := m.Ensure(ctx, mB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dA); !os.IsNotExist(err) {
		t.Fatal("the released sibling A was not consumed when it became the victim")
	}
	if !bytes.Equal(readFile(t, filepath.Join(dB2, "model.bin")), aligned) {
		t.Fatal("B (second time) is wrong")
	}
}

// TestVictimNeedsNoIdleWindow: with a capacity, the eviction victim is consumed as soon as no
// lease holds it, even if it was used a moment ago and the idle window is long.
func TestVictimNeedsNoIdleWindow(t *testing.T) {
	ctx := context.Background()
	e, root := setup(t)
	base, aligned, _ := variants()
	mA, _ := e.Publish(ctx, publish(t, e, root, "a", base))
	mB, _ := e.Publish(ctx, publish(t, e, root, "b", aligned))
	m := &csi.Materializer{Root: filepath.Join(root, "node"), Pull: e.Pull, RebaseIdle: time.Hour,
		MaxArtifacts: 1}
	if err := m.Init(); err != nil {
		t.Fatal(err)
	}
	dA, err := m.Ensure(ctx, mA)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire("vol-a", mA, "/pods/a"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := m.Release("vol-a"); err != nil {
		t.Fatal(err)
	}
	dB, err := m.Ensure(ctx, mB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dA); !os.IsNotExist(err) {
		t.Fatal("the just-released victim was not consumed")
	}
	if !bytes.Equal(readFile(t, filepath.Join(dB, "model.bin")), aligned) {
		t.Fatal("B is wrong")
	}
}
