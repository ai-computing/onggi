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

// variants returns a base and an aligned sibling (a middle region rewritten in place) and a shifted
// sibling (a header inserted at the front, so every later byte moves).
func variants() (base, aligned, shifted []byte) {
	r := rand.New(rand.NewSource(7))
	base = make([]byte, 6<<20)
	r.Read(base)
	aligned = append([]byte(nil), base...)
	r.Read(aligned[3<<20 : 3<<20+300<<10])
	shifted = append(append(make([]byte, 0, len(base)+4096), bytes.Repeat([]byte{9}, 4096)...), base...)
	return
}

func publish(t *testing.T, e *Engine, root, name string, data []byte) (dir string) {
	t.Helper()
	dir = filepath.Join(root, "src-"+name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "model.bin"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func setup(t *testing.T) (*Engine, string) {
	t.Helper()
	root := t.TempDir()
	e, err := New(filepath.Join(root, "store"), filepath.Join(root, "index"), filepath.Join(root, "attest"), 4)
	if err != nil {
		t.Fatal(err)
	}
	return e, root
}

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPaths(t *testing.T) {
	ctx := context.Background()
	e, root := setup(t)
	base, aligned, shifted := variants()
	mA, err := e.Publish(ctx, publish(t, e, root, "a", base))
	if err != nil {
		t.Fatal(err)
	}
	mB, _ := e.Publish(ctx, publish(t, e, root, "b", aligned))
	mC, _ := e.Publish(ctx, publish(t, e, root, "c", shifted))

	// Reconstruct: no offer
	dA := filepath.Join(root, "art", "A")
	os.MkdirAll(dA, 0o755)
	st, err := e.Pull(ctx, mA, dA, nil)
	if err != nil || !bytes.Equal(readFile(t, filepath.Join(dA, "model.bin")), base) || st.Base != "" {
		t.Fatalf("reconstruct: %v %+v", err, st)
	}

	// Keep, trusted: A is attested unchanged, so kept chunks are not read back
	claimed := false
	offer := &csi.Offer{Bases: []store.Base{{Manifest: mA, Dir: dA}},
		Claim: func(b store.Base) (string, bool) { claimed = true; return b.Dir, true }}
	dB := filepath.Join(root, "art", "B")
	os.MkdirAll(dB, 0o755)
	st, err = e.Pull(ctx, mB, dB, offer)
	if err != nil || !claimed || st.Base != mA.String() || st.Verified != 0 || st.Reused < int64(len(aligned))*8/10 {
		t.Fatalf("keep trusted: %v claimed=%v %+v", err, claimed, st)
	}
	if !bytes.Equal(readFile(t, filepath.Join(dB, "model.bin")), aligned) {
		t.Fatal("keep trusted: output differs")
	}
	if _, err := os.Stat(filepath.Join(dA, "model.bin")); !os.IsNotExist(err) {
		t.Fatal("keep must consume the base file")
	}

	// Keep, tampered: B is changed after it was written; the attestation no longer matches, so kept
	// chunks are read back and the corrupted one is fetched again
	p := filepath.Join(dB, "model.bin")
	os.Chmod(p, 0o644)
	f, _ := os.OpenFile(p, os.O_RDWR, 0)
	f.WriteAt([]byte("corrupt"), 100)
	f.Close()
	future := time.Now().Add(time.Second)
	os.Chtimes(p, future, future)
	dA2 := filepath.Join(root, "art", "A2")
	os.MkdirAll(dA2, 0o755)
	st, err = e.Pull(ctx, mA, dA2, &csi.Offer{Bases: []store.Base{{Manifest: mB, Dir: dB}},
		Claim: func(b store.Base) (string, bool) { return b.Dir, true }})
	if err != nil || st.Verified == 0 || st.Repaired == 0 {
		t.Fatalf("keep tampered: %v %+v", err, st)
	}
	if !bytes.Equal(readFile(t, filepath.Join(dA2, "model.bin")), base) {
		t.Fatal("keep tampered: output differs")
	}

	// a shifted sibling keeps nothing at the same offset: no claim, assembled from the store
	claimed = false
	dC := filepath.Join(root, "art", "C")
	os.MkdirAll(dC, 0o755)
	st, err = e.Pull(ctx, mC, dC, &csi.Offer{Bases: []store.Base{{Manifest: mA, Dir: dA2}},
		Claim: func(b store.Base) (string, bool) { claimed = true; return b.Dir, true }})
	if err != nil || claimed || st.BaseBest != 0 || !bytes.Equal(readFile(t, filepath.Join(dC, "model.bin")), shifted) {
		t.Fatalf("shifted: %v claimed=%v %+v", err, claimed, st)
	}

	// Copy: A2 stays intact and seeds B
	held := false
	dB2 := filepath.Join(root, "art", "B2")
	os.MkdirAll(dB2, 0o755)
	st, err = e.Pull(ctx, mB, dB2, &csi.Offer{Readable: []store.Base{{Manifest: mA, Dir: dA2}}, BaseCopy: store.BaseCopyAlways,
		Hold: func(store.Base) (func(), bool) { held = true; return func() {}, true }})
	if err != nil || !held || st.LocalBase != mA.String() || !bytes.Equal(readFile(t, filepath.Join(dB2, "model.bin")), aligned) {
		t.Fatalf("copy: %v held=%v %+v", err, held, st)
	}
	if !bytes.Equal(readFile(t, filepath.Join(dA2, "model.bin")), base) {
		t.Fatal("copy must leave the base intact")
	}
	if err := e.Verify(ctx, mB, dB2); err != nil {
		t.Fatal(err)
	}
}

func TestSameOffset(t *testing.T) {
	e, root := setup(t)
	base, aligned, _ := variants()
	ctx := context.Background()
	mA, _ := e.Publish(ctx, publish(t, e, root, "a", base))
	mB, _ := e.Publish(ctx, publish(t, e, root, "b", aligned))
	_, ia, _ := e.Load(mA)
	_, ib, _ := e.Load(mB)
	_, same := sameOffset(ia[0].Chunks, ib[0].Chunks)
	if same < int64(len(aligned))*8/10 || same == int64(len(aligned)) {
		t.Fatalf("same=%d of %d", same, len(aligned))
	}
}

// TestKeepRenamed: variants of different shapes name their files differently; Keep pairs the
// target file with the base file that holds most of it at the same offset.
func TestKeepRenamed(t *testing.T) {
	ctx := context.Background()
	e, root := setup(t)
	base, aligned, _ := variants()
	write := func(name, file string, data []byte) string {
		d := filepath.Join(root, "src-"+name)
		os.MkdirAll(d, 0o755)
		if err := os.WriteFile(filepath.Join(d, file), data, 0o644); err != nil {
			t.Fatal(err)
		}
		return d
	}
	mA, err := e.Publish(ctx, write("a", "decode_B2.bin", base))
	if err != nil {
		t.Fatal(err)
	}
	mB, err := e.Publish(ctx, write("b", "decode_B8.bin", aligned))
	if err != nil {
		t.Fatal(err)
	}
	dA := filepath.Join(root, "art", "A")
	os.MkdirAll(dA, 0o755)
	if _, err := e.Pull(ctx, mA, dA, nil); err != nil {
		t.Fatal(err)
	}
	offer := &csi.Offer{Bases: []store.Base{{Manifest: mA, Dir: dA}},
		Claim: func(b store.Base) (string, bool) { return b.Dir, true }}
	dB := filepath.Join(root, "art", "B")
	os.MkdirAll(dB, 0o755)
	st, err := e.Pull(ctx, mB, dB, offer)
	if err != nil || st.Base != mA.String() || st.Verified != 0 || st.Reused < int64(len(aligned))*8/10 {
		t.Fatalf("keep renamed: %v %+v", err, st)
	}
	if !bytes.Equal(readFile(t, filepath.Join(dB, "decode_B8.bin")), aligned) {
		t.Fatal("keep renamed: output differs")
	}
	if _, err := os.Stat(filepath.Join(dA, "decode_B2.bin")); !os.IsNotExist(err) {
		t.Fatal("keep renamed must consume the base file")
	}
}
