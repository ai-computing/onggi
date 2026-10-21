// Package backendtest is the conformance suite every backend driver must pass (ADR-0008).
package backendtest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"sort"
	"sync"
	"testing"

	"github.com/ai-computing/onggi/pkg/backend"
	"github.com/ai-computing/onggi/pkg/digest"
)

func data(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func key(kind backend.Kind, b []byte) backend.Key {
	return backend.Key{Kind: kind, Name: digest.Of(b).Hex()}
}

func get(t *testing.T, b backend.Backend, k backend.Key, off, n int64) []byte {
	t.Helper()
	r, err := b.Get(context.Background(), k, off, n)
	if err != nil {
		t.Fatalf("get %s: %v", k, err)
	}
	defer r.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read %s: %v", k, err)
	}
	return out
}

type failingReader struct {
	b   []byte
	pos int
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.b)/2 {
		return 0, errors.New("source failed")
	}
	n := copy(p, r.b[r.pos:len(r.b)/2])
	r.pos += n
	return n, nil
}

// Run checks a backend. newBackend must return an empty backend each call.
func Run(t *testing.T, newBackend func(t *testing.T) backend.Backend) {
	ctx := context.Background()

	t.Run("PutGetRoundTrip", func(t *testing.T) {
		b := newBackend(t)
		for i, n := range []int{0, 1, 4096, 3<<20 + 7} {
			d := data(int64(i), n)
			k := key(backend.Pack, d)
			if err := b.Put(ctx, k, bytes.NewReader(d), int64(n)); err != nil {
				t.Fatal(err)
			}
			if got := get(t, b, k, 0, -1); !bytes.Equal(got, d) {
				t.Fatalf("size %d: read back differs", n)
			}
		}
	})

	t.Run("PutIsIdempotent", func(t *testing.T) {
		b := newBackend(t)
		d := data(10, 100_000)
		k := key(backend.Index, d)
		for i := 0; i < 3; i++ {
			if err := b.Put(ctx, k, bytes.NewReader(d), int64(len(d))); err != nil {
				t.Fatal(err)
			}
		}
		if !bytes.Equal(get(t, b, k, 0, -1), d) {
			t.Fatal("content changed after repeated Put")
		}
	})

	t.Run("ConcurrentPutSameKey", func(t *testing.T) {
		b := newBackend(t)
		d := data(11, 2<<20)
		k := key(backend.Pack, d)
		var wg sync.WaitGroup
		errs := make(chan error, 16)
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs <- b.Put(ctx, k, bytes.NewReader(d), int64(len(d)))
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		if !bytes.Equal(get(t, b, k, 0, -1), d) {
			t.Fatal("concurrent writers left wrong bytes")
		}
	})

	t.Run("FailedPutLeavesNothing", func(t *testing.T) {
		b := newBackend(t)
		d := data(12, 1<<20)
		k := key(backend.Pack, d)
		if err := b.Put(ctx, k, &failingReader{b: d}, int64(len(d))); err == nil {
			t.Fatal("Put with a failing source succeeded")
		}
		if has, _ := b.Has(ctx, []backend.Key{k}); has[0] {
			t.Fatal("a failed Put left the key visible")
		}
		short := d[:1000]
		if err := b.Put(ctx, k, bytes.NewReader(short), int64(len(d))); err == nil {
			t.Fatal("Put with fewer bytes than size succeeded")
		}
		if has, _ := b.Has(ctx, []backend.Key{k}); has[0] {
			t.Fatal("a short Put left the key visible")
		}
	})

	t.Run("RangeReads", func(t *testing.T) {
		b := newBackend(t)
		d := data(13, 1<<20)
		k := key(backend.Pack, d)
		if err := b.Put(ctx, k, bytes.NewReader(d), int64(len(d))); err != nil {
			t.Fatal(err)
		}
		for _, r := range [][2]int64{{0, 10}, {12345, 4096}, {int64(len(d)) - 5, 5}, {500, -1}} {
			want := d[r[0]:]
			if r[1] >= 0 {
				want = d[r[0] : r[0]+r[1]]
			}
			if got := get(t, b, k, r[0], r[1]); !bytes.Equal(got, want) {
				t.Fatalf("range %v: wrong bytes", r)
			}
		}
	})

	t.Run("MissingKey", func(t *testing.T) {
		b := newBackend(t)
		k := backend.Key{Kind: backend.Manifest, Name: digest.Of([]byte("absent")).Hex()}
		if _, err := b.Get(ctx, k, 0, -1); !errors.Is(err, backend.ErrNotFound) {
			t.Fatalf("Get of a missing key: %v, want ErrNotFound", err)
		}
		if err := b.Delete(ctx, k); err != nil {
			t.Fatalf("Delete of a missing key: %v", err)
		}
	})

	t.Run("HasListDelete", func(t *testing.T) {
		b := newBackend(t)
		var want []string
		var keys []backend.Key
		for i := 0; i < 40; i++ {
			d := data(int64(100+i), 100+i)
			k := key(backend.List, d)
			if err := b.Put(ctx, k, bytes.NewReader(d), int64(len(d))); err != nil {
				t.Fatal(err)
			}
			keys = append(keys, k)
			want = append(want, k.Name)
		}
		other := data(999, 10)
		if err := b.Put(ctx, key(backend.Pack, other), bytes.NewReader(other), 10); err != nil {
			t.Fatal(err)
		}
		has, err := b.Has(ctx, append(keys, key(backend.List, other)))
		if err != nil {
			t.Fatal(err)
		}
		for i, h := range has[:40] {
			if !h {
				t.Fatalf("Has(%s) = false", keys[i])
			}
		}
		if has[40] {
			t.Fatal("Has reports a key of another kind")
		}
		var got []string
		if err := b.List(ctx, backend.List, func(name string, size int64) error {
			got = append(got, name)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		sort.Strings(got)
		sort.Strings(want)
		if len(got) != len(want) {
			t.Fatalf("List returned %d keys, want %d", len(got), len(want))
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("List: %s, want %s", got[i], want[i])
			}
		}
		if err := b.Delete(ctx, keys[0]); err != nil {
			t.Fatal(err)
		}
		if has, _ := b.Has(ctx, keys[:1]); has[0] {
			t.Fatal("key still present after Delete")
		}
	})
}

// RunRefs checks a ref store.
func RunRefs(t *testing.T, newRefs func(t *testing.T) backend.Refs) {
	ctx := context.Background()
	t.Run("CompareAndSwap", func(t *testing.T) {
		r := newRefs(t)
		name := "llama-3.1-8b@atom/decode-s512-b8"
		if _, err := r.GetRef(ctx, name); !errors.Is(err, backend.ErrNotFound) {
			t.Fatalf("missing ref: %v", err)
		}
		if err := r.SetRef(ctx, name, "", "sha256:a"); err != nil {
			t.Fatal(err)
		}
		if err := r.SetRef(ctx, name, "", "sha256:b"); !errors.Is(err, backend.ErrConflict) {
			t.Fatalf("create over an existing ref: %v, want ErrConflict", err)
		}
		if err := r.SetRef(ctx, name, "sha256:a", "sha256:b"); err != nil {
			t.Fatal(err)
		}
		if v, _ := r.GetRef(ctx, name); v != "sha256:b" {
			t.Fatalf("ref = %q", v)
		}
		seen := map[string]string{}
		if err := r.ListRefs(ctx, func(n, v string) error { seen[n] = v; return nil }); err != nil {
			t.Fatal(err)
		}
		if seen[name] != "sha256:b" || len(seen) != 1 {
			t.Fatalf("ListRefs = %v", seen)
		}
	})
	t.Run("ConcurrentSwapsOneWins", func(t *testing.T) {
		r := newRefs(t)
		if err := r.SetRef(ctx, "x", "", "v0"); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		wins := 0
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if r.SetRef(ctx, "x", "v0", string(rune('a'+i))) == nil {
					mu.Lock()
					wins++
					mu.Unlock()
				}
			}(i)
		}
		wg.Wait()
		if wins != 1 {
			t.Fatalf("%d concurrent swaps from the same old value succeeded, want 1", wins)
		}
	})
}
