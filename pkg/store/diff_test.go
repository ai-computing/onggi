package store

import (
	"bytes"
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
	st, err := s.Diff(ctx, base, pushDir(t, s, map[string][]byte{"decode_B8.rbln": w, "config.json": []byte(`{"batch": 2}`)}), DiffOptions{})
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
	st, err = s.Diff(ctx, base, pushDir(t, s, map[string][]byte{"decode_B2.rbln": v}), DiffOptions{Ranges: true})
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

	// the ranges cover exactly the bytes that are not at the same offset, in order (#46)
	var covered, end int64
	for _, r := range f.Changed {
		if r[0] < end || r[1] <= 0 || r[0]+r[1] > f.Size {
			t.Fatalf("ranges out of order or out of the file: %v", f.Changed)
		}
		covered, end = covered+r[1], r[0]+r[1]
	}
	if len(f.Changed) != f.ChangedRuns || covered != f.Size-f.Same || int64(len(v)) != f.Size {
		t.Fatalf("ranges %v cover %d B, want %d", f.Changed, covered, f.Size-f.Same)
	}
	if !bytes.Equal(v[:f.Changed[0][0]], w[:f.Changed[0][0]]) {
		t.Fatal("bytes before the first changed range differ from the base")
	}

	// the same bytes rearranged in 32 KiB blocks: most found elsewhere in the base, little at the
	// same offset; sub-chunks that straddle block boundaries are new
	st, err = s.Diff(ctx, base, pushDir(t, s, map[string][]byte{"decode_B2.rbln": shuffled(w, 23)}), DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	f = st.Files[0]
	t.Logf("rearranged: %+v", f)
	if f.Same+f.Moved+f.New != f.Size || f.Moved < f.Size/2 || f.Same > f.Size/10 || f.Changed != nil {
		t.Fatalf("rearranged: %+v", f)
	}
}

// The range plan (ADR-0010) covers every file in order with runs of one kind; an elsewhere run
// names base bytes equal to its own, and the sums match the totals.
func TestRangePlan(t *testing.T) {
	ctx := context.Background()
	s := openTwoLevel(t, t.TempDir())
	w := random(31, 8<<20)
	bm := pushDir(t, s, map[string][]byte{"decode_B2.rbln": w})
	v := append([]byte(nil), w...)
	copy(v[5<<20:], random(32, 100<<10))
	r := shuffled(w, 33)
	nm := pushDir(t, s, map[string][]byte{"decode_B2.rbln": v, "prefill.rbln": r})
	st, err := s.Diff(ctx, bm, nm, DiffOptions{Ranges: true})
	if err != nil {
		t.Fatal(err)
	}
	if st.Schema != SwitchPlanSchema || st.BaseManifest != bm.String() || st.Manifest != nm.String() {
		t.Fatalf("plan header: %q %q %q", st.Schema, st.BaseManifest, st.Manifest)
	}
	data := map[string][]byte{"decode_B2.rbln": v, "prefill.rbln": r}
	kinds := map[string]int{}
	for _, f := range st.Files {
		var end int64
		sum := map[string]int64{}
		for i, rg := range f.Ranges {
			if rg.Offset != end || rg.Length <= 0 {
				t.Fatalf("%s: range %d at %d, want %d", f.Path, i, rg.Offset, end)
			}
			if i > 0 && f.Ranges[i-1].Kind == rg.Kind && rg.Kind != RangeElsewhere {
				t.Fatalf("%s: adjacent %s ranges not merged", f.Path, rg.Kind)
			}
			end += rg.Length
			sum[rg.Kind] += rg.Length
			kinds[rg.Kind]++
			got := data[f.Path][rg.Offset : rg.Offset+rg.Length]
			switch rg.Kind {
			case RangeSame:
				if !bytes.Equal(got, w[rg.Offset:rg.Offset+rg.Length]) {
					t.Fatalf("%s: same range at %d differs from the base", f.Path, rg.Offset)
				}
			case RangeElsewhere:
				if rg.SourceFile != "decode_B2.rbln" || rg.SourceOffset == nil {
					t.Fatalf("%s: elsewhere range without a source: %+v", f.Path, rg)
				}
				if !bytes.Equal(got, w[*rg.SourceOffset:*rg.SourceOffset+rg.Length]) {
					t.Fatalf("%s: elsewhere range at %d differs from its source", f.Path, rg.Offset)
				}
			case RangeNew:
				if rg.SourceOffset != nil {
					t.Fatalf("%s: new range with a source", f.Path)
				}
			default:
				t.Fatalf("unknown kind %q", rg.Kind)
			}
		}
		if end != f.Size || sum[RangeSame] != f.Same || sum[RangeElsewhere] != f.Moved || sum[RangeNew] != f.New {
			t.Fatalf("%s: plan covers %d of %d B; sums %v vs %d/%d/%d", f.Path, end, f.Size, sum, f.Same, f.Moved, f.New)
		}
	}
	if kinds[RangeSame] == 0 || kinds[RangeElsewhere] == 0 || kinds[RangeNew] == 0 {
		t.Fatalf("kinds %v; the test should produce all three", kinds)
	}
	// without --ranges the plan is not listed, and the old fields are unchanged
	st2, err := s.Diff(ctx, bm, nm, DiffOptions{})
	if err != nil || st2.Files[0].Ranges != nil || st2.Files[0].Changed != nil || st2.Same != st.Same || st2.Schema != SwitchPlanSchema {
		t.Fatalf("without ranges: %+v %v", st2.Files[0], err)
	}
}
