package csi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ai-computing/onggi/pkg/store"
)

// logs collects a materializer's log lines.
type logs struct {
	mu    sync.Mutex
	lines []string
}

func (l *logs) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logs) has(s string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, x := range l.lines {
		if strings.Contains(x, s) {
			return true
		}
	}
	return false
}

// An idle artifact is turned into the next one; then back again (#41).
func TestEnsureRebasesFromIdleArtifact(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	m, c := newMaterializer(t, f)
	m.RebaseIdle = time.Nanosecond
	var unmounted []string
	m.Unmount = func(dir string) error { unmounted = append(unmounted, dir); return nil }

	if _, err := m.Ensure(ctx, f.a); err != nil {
		t.Fatal(err)
	}
	dirA := m.Dir(f.a)
	dir, err := m.Ensure(ctx, f.b)
	if err != nil {
		t.Fatal(err)
	}
	sameTree(t, f.srcB, dir)
	st := c.pulls[1]
	t.Logf("B from A: %+v", st)
	if st.Base != f.a.String() || st.Reused < 3<<20*9/10 || len(unmounted) != 1 || unmounted[0] != dirA {
		t.Fatalf("B did not take over A: %+v, unmounted %v", st, unmounted)
	}
	if isDir(dirA) {
		t.Fatal("A is still materialized after being consumed")
	}
	if ents, _ := os.ReadDir(filepath.Join(m.Root, "tmp")); len(ents) != 0 {
		t.Fatalf("tmp holds %d entries", len(ents))
	}
	// A again: B is idle now, so A comes back from it
	dir, err = m.Ensure(ctx, f.a)
	if err != nil {
		t.Fatal(err)
	}
	sameTree(t, f.srcA, dir)
	if st := c.pulls[2]; st.Base != f.b.String() {
		t.Fatalf("A did not take over B: %+v", st)
	}
}

// An artifact a volume uses, or one used recently, is never consumed.
func TestEnsureKeepsBusyArtifacts(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	m, c := newMaterializer(t, f)
	m.RebaseIdle = time.Hour
	var l logs
	m.Logf = l.logf
	if _, err := m.Ensure(ctx, f.a); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Ensure(ctx, f.b); err != nil { // A was used just now
		t.Fatal(err)
	}
	if c.pulls[1].Base != "" || !isDir(m.Dir(f.a)) {
		t.Fatalf("took a recently used artifact: %+v", c.pulls[1])
	}
	if !l.has("no artifact to take over (0 in use, 1 used within 1h0m0s)") {
		t.Fatalf("no reason logged: %q", l.lines)
	}

	m2, c2 := newMaterializer(t, f)
	m2.RebaseIdle = time.Nanosecond
	if _, err := m2.Ensure(ctx, f.a); err != nil {
		t.Fatal(err)
	}
	if err := m2.Acquire("csi-vol1", f.a, "/target"); err != nil {
		t.Fatal(err)
	}
	if _, err := m2.Ensure(ctx, f.b); err != nil {
		t.Fatal(err)
	}
	if c2.pulls[1].Base != "" || !isDir(m2.Dir(f.a)) {
		t.Fatalf("took an artifact in use: %+v", c2.pulls[1])
	}
	// in use, then released: idle at once with RebaseIdle 1 ns, but too different from a third
	// artifact to be worth taking over
	if _, _, _, err := m2.Release("csi-vol1"); err != nil {
		t.Fatal(err)
	}
	var l2 logs
	m2.Logf = l2.logf
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "model.bin"), random(9, 1<<20), 0o644)
	other, _, err := f.s.Push(ctx, src, store.PushOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m2.Ensure(ctx, other); err != nil {
		t.Fatal(err)
	}
	if !l2.has("took over nothing: of 2 idle artifacts the best keeps 0 of") {
		t.Fatalf("no reason logged: %q", l2.lines)
	}
}
