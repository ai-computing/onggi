package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ai-computing/onggi/pkg/digest"
)

func pushDir(t *testing.T, s *Store, files map[string][]byte) digest.Digest {
	t.Helper()
	src := t.TempDir()
	for n, b := range files {
		write(t, filepath.Join(src, n), b)
	}
	m, _, err := s.Push(context.Background(), src, PushOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestDiff(t *testing.T) {
	ctx := context.Background()
	s := openTwoLevel(t, t.TempDir())
	w := random(21, 8<<20)
	base := pushDir(t, s, map[string][]byte{"decode_B2.rbln": w, "config.json": []byte(`{"batch": 2}`)})

	// identical content under another name: all Same, paired by kind
	st, err := s.Diff(ctx, base, pushDir(t, s, map[string][]byte{"decode_B8.rbln": w, "config.json": []byte(`{"batch": 2}`)}))
	if err != nil {
		t.Fatal(err)
	}
	if st.Same != st.Size || st.SameBlocks != st.Size || st.New != 0 || st.ChangedRuns != 0 || st.NewStored != 0 {
		t.Fatalf("identical: %+v", st)
	}
	if st.Files[0].Base == "" || st.Files[1].Base == "" {
		t.Fatalf("not paired: %+v", st.Files)
	}

	// one region overwritten in place
	v := append([]byte(nil), w...)
	copy(v[3<<20:], random(22, 200<<10))
	st, err = s.Diff(ctx, base, pushDir(t, s, map[string][]byte{"decode_B2.rbln": v}))
	if err != nil {
		t.Fatal(err)
	}
	f := st.Files[0]
	t.Logf("in place: %+v", f)
	// sub-chunks next to the region may resynchronize and split it into a few runs
	if f.Same+f.Moved+f.New != f.Size || f.New < 200<<10 || f.New > 600<<10 || f.ChangedRuns < 1 || f.ChangedRuns > 3 ||
		f.LargestRun < 200<<10 || f.SameBlocks > f.Same {
		t.Fatalf("in place: %+v", f)
	}
	if st.NewStored == 0 {
		t.Fatalf("no new stored bytes: %+v", st)
	}

	// the same bytes rearranged: nothing new, little at the same offset
	st, err = s.Diff(ctx, base, pushDir(t, s, map[string][]byte{"decode_B2.rbln": shuffled(w, 23)}))
	if err != nil {
		t.Fatal(err)
	}
	f = st.Files[0]
	t.Logf("rearranged: %+v", f)
	if f.Same+f.Moved+f.New != f.Size || f.Moved < f.Size/2 || f.New > f.Size/10 {
		t.Fatalf("rearranged: %+v", f)
	}
}
