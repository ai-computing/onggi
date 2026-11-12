package casync

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ai-computing/onggi/pkg/csi"
)

// TestKeepRepairsChangedBase: a base that was modified after the node wrote it no longer matches
// its record, so Keep reads the kept chunks back and replaces the ones that fail their digest.
func TestKeepRepairsChangedBase(t *testing.T) {
	ctx := context.Background()
	e, root := setup(t)
	base, aligned, _ := variants()
	mA, _ := e.Publish(ctx, publish(t, e, root, "a", base))
	mB, _ := e.Publish(ctx, publish(t, e, root, "b", aligned))
	m := &csi.Materializer{Root: filepath.Join(root, "node"), Pull: e.Pull, RebaseIdle: time.Nanosecond, MaxArtifacts: 1}
	if err := m.Init(); err != nil {
		t.Fatal(err)
	}
	dA, err := m.Ensure(ctx, mA)
	if err != nil {
		t.Fatal(err)
	}
	// damage a region that A and B share (B differs from A only around 3 MiB)
	time.Sleep(20 * time.Millisecond)
	f, err := os.OpenFile(filepath.Join(dA, "model.bin"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(bytes.Repeat([]byte{0xEE}, 64<<10), 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	dB, err := m.Ensure(ctx, mB)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readFile(t, filepath.Join(dB, "model.bin")), aligned) {
		t.Fatal("Keep carried a changed base into the target")
	}
	if _, err := os.Stat(dA); !os.IsNotExist(err) {
		t.Fatal("the idle victim was not consumed")
	}
}

// TestHardLinkedBaseIsNotOverwritten: a base whose file has a second name is never rebuilt in
// place, so the bytes behind the second name stay those of the base.
func TestHardLinkedBaseIsNotOverwritten(t *testing.T) {
	ctx := context.Background()
	e, root := setup(t)
	base, aligned, _ := variants()
	mA, _ := e.Publish(ctx, publish(t, e, root, "a", base))
	mB, _ := e.Publish(ctx, publish(t, e, root, "b", aligned))
	m := &csi.Materializer{Root: filepath.Join(root, "node"), Pull: e.Pull, RebaseIdle: time.Nanosecond, MaxArtifacts: 1}
	if err := m.Init(); err != nil {
		t.Fatal(err)
	}
	dA, err := m.Ensure(ctx, mA)
	if err != nil {
		t.Fatal(err)
	}
	snap := filepath.Join(m.Root, "snapshot-model.bin")
	if err := os.Link(filepath.Join(dA, "model.bin"), snap); err != nil {
		t.Skipf("hard links unavailable here: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	dB, err := m.Ensure(ctx, mB)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readFile(t, filepath.Join(dB, "model.bin")), aligned) {
		t.Fatal("B is wrong")
	}
	if !bytes.Equal(readFile(t, snap), base) {
		t.Fatal("a hard-linked base was rebuilt in place")
	}
}

// TestLeasedBaseSurvivesConcurrentPulls: while a volume holds A, several pulls of B run at once;
// A must stay intact and every pull must return the same correct B.
func TestLeasedBaseSurvivesConcurrentPulls(t *testing.T) {
	ctx := context.Background()
	e, root := setup(t)
	base, aligned, _ := variants()
	mA, _ := e.Publish(ctx, publish(t, e, root, "a", base))
	mB, _ := e.Publish(ctx, publish(t, e, root, "b", aligned))
	m := &csi.Materializer{Root: filepath.Join(root, "node"), Pull: e.Pull, RebaseIdle: time.Nanosecond, MaxArtifacts: 2}
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
	errs := make(chan error, 4)
	dirs := make(chan string, 4)
	for i := 0; i < 4; i++ {
		go func() {
			d, err := m.Ensure(ctx, mB)
			dirs <- d
			errs <- err
		}()
	}
	for i := 0; i < 4; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		if d := <-dirs; !bytes.Equal(readFile(t, filepath.Join(d, "model.bin")), aligned) {
			t.Fatal("a concurrent pull returned a wrong B")
		}
	}
	if !bytes.Equal(readFile(t, filepath.Join(dA, "model.bin")), base) {
		t.Fatal("a leased base changed under concurrent pulls")
	}
}
