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
	// A released and idle, B idle: C consumes the least recently used idle sibling (A)
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
	_, errA := os.Stat(dA)
	_, errB := os.Stat(dB)
	if errA == nil && errB == nil {
		t.Fatal("C consumed no idle sibling")
	}
	if errB != nil {
		if !bytes.Equal(readFile(t, filepath.Join(dA, "model.bin")), base) {
			t.Fatal("A changed although B was consumed")
		}
	}
}
