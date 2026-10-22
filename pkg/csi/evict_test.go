package csi

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// An artifact with a volume, or used recently, stays; an idle one with no volume goes, its
// self-bind is unmounted first, and a later request materializes it again.
func TestEvictIdleArtifacts(t *testing.T) {
	f := newFixture(t)
	m, c := newMaterializer(t, f)
	ctx := context.Background()
	for _, d := range []string{"a", "b"} {
		dg := f.a
		if d == "b" {
			dg = f.b
		}
		if _, err := m.Ensure(ctx, dg); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Acquire("csi-vol-a", f.a, "/pods/x/vol"); err != nil {
		t.Fatal(err)
	}
	var unmounted []string
	unmount := func(dir string) error { unmounted = append(unmounted, dir); return nil }

	// recently used: nothing goes
	if gone, err := m.Evict(time.Now(), time.Hour, unmount); err != nil || len(gone) != 0 {
		t.Fatalf("evicted recently used artifacts: %v %v", gone, err)
	}
	// an hour later: b (no volume) goes, a (a volume holds it) stays
	gone, err := m.Evict(time.Now().Add(2*time.Hour), time.Hour, unmount)
	if err != nil || len(gone) != 1 || gone[0] != f.b {
		t.Fatalf("evicted %v %v, want only b", gone, err)
	}
	if len(unmounted) != 1 || unmounted[0] != m.Dir(f.b) {
		t.Fatalf("unmounted %v, want b's directory", unmounted)
	}
	if isDir(m.Dir(f.b)) || !isDir(m.Dir(f.a)) {
		t.Fatal("wrong directories left")
	}
	ents, _ := os.ReadDir(filepath.Join(m.Root, "tmp"))
	if len(ents) != 0 {
		t.Fatalf("eviction left %d entries in tmp", len(ents))
	}
	ds, _ := m.Materialized()
	if len(ds) != 1 || ds[0] != f.a {
		t.Fatalf("materialized %v", ds)
	}
	// once a's volume goes and a is idle, it goes too
	if _, _, _, err := m.Release("csi-vol-a"); err != nil {
		t.Fatal(err)
	}
	if gone, _ := m.Evict(time.Now().Add(30*time.Minute), time.Hour, unmount); len(gone) != 0 {
		t.Fatal("evicted an artifact released 0 s ago")
	}
	if gone, _ := m.Evict(time.Now().Add(2*time.Hour), time.Hour, unmount); len(gone) != 1 || gone[0] != f.a {
		t.Fatalf("a not evicted: %v", gone)
	}
	// b is materialized again on demand, from the node cache
	pulls := len(c.pulls)
	if _, err := m.Ensure(ctx, f.b); err != nil {
		t.Fatal(err)
	}
	sameTree(t, f.srcB, m.Dir(f.b))
	if len(c.pulls) != pulls+1 || c.pulls[len(c.pulls)-1].ReadBytes != 0 {
		t.Fatalf("re-materialization: %d pulls, last read %d B from the store", len(c.pulls)-pulls, c.pulls[len(c.pulls)-1].ReadBytes)
	}
}

// A directory whose self-bind cannot be unmounted is left alone.
func TestEvictSkipsWhatCannotBeUnmounted(t *testing.T) {
	f := newFixture(t)
	m, _ := newMaterializer(t, f)
	if _, err := m.Ensure(context.Background(), f.a); err != nil {
		t.Fatal(err)
	}
	gone, err := m.Evict(time.Now().Add(2*time.Hour), time.Hour, func(string) error { return os.ErrPermission })
	if err != nil || len(gone) != 0 || !isDir(m.Dir(f.a)) {
		t.Fatalf("evicted %v %v despite a failed unmount", gone, err)
	}
}
