package csi

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ai-computing/onggi/pkg/digest"
)

func TestAcquireRecordFailureDoesNotLeakLease(t *testing.T) {
	m := &Materializer{Root: t.TempDir()}
	if err := m.Init(); err != nil {
		t.Fatal(err)
	}
	d := digest.Of([]byte("artifact"))
	// Fail the volume-record write after the artifact lease has been written.
	blocked := filepath.Join(m.Root, "volumes", "volume.json.tmp")
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire("volume", d, "/target"); err == nil {
		t.Fatal("want record failure")
	}
	if n, err := m.Users(d); err != nil || n != 0 {
		t.Fatalf("failed acquisition left %d leases: %v", n, err)
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire("volume", d, "/target"); err != nil {
		t.Fatal(err)
	}
	if _, n, ok, err := m.Release("volume"); err != nil || !ok || n != 0 {
		t.Fatalf("retry/release: %d %v %v", n, ok, err)
	}
}

func TestAcquireRejectsChangingPublishedIdentity(t *testing.T) {
	m := &Materializer{Root: t.TempDir()}
	if err := m.Init(); err != nil {
		t.Fatal(err)
	}
	a, b := digest.Of([]byte("a")), digest.Of([]byte("b"))
	if err := m.Acquire("volume", a, "/target"); err != nil {
		t.Fatal(err)
	}
	for _, v := range []struct {
		d      digest.Digest
		target string
	}{{b, "/target"}, {a, "/other"}} {
		if err := m.Acquire("volume", v.d, v.target); err == nil {
			t.Fatal("changed volume identity accepted")
		}
	}
	if n, _ := m.Users(a); n != 1 {
		t.Fatalf("original lease lost: %d", n)
	}
	if n, _ := m.Users(b); n != 0 {
		t.Fatalf("foreign lease created: %d", n)
	}
	if got, _, ok, err := m.Release("volume"); err != nil || !ok || got != a {
		t.Fatalf("original volume changed: %v %v %v", got, ok, err)
	}
}

func TestAcquireRetryFailurePreservesExistingLease(t *testing.T) {
	m := &Materializer{Root: t.TempDir()}
	if err := m.Init(); err != nil {
		t.Fatal(err)
	}
	d := digest.Of([]byte("artifact"))
	if err := m.Acquire("volume", d, "/target"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(m.Root, "volumes", "volume.json.tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = m.Acquire("volume", d, "/target")
	if n, err := m.Users(d); err != nil || n != 1 {
		t.Fatalf("existing lease removed: %d %v", n, err)
	}
}
