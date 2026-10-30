package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden switch plans of conformance/binding")

// goldenData is the generator of conformance/binding/golden_data.py: SHA-256 in counter mode, so
// Go and Python produce the same bytes.
func goldenData(seed, n int) []byte {
	var out []byte
	for i := 0; len(out) < n; i++ {
		h := sha256.Sum256([]byte(fmt.Sprintf("onggi-conformance:%d:%d", seed, i)))
		out = append(out, h[:]...)
	}
	return out[:n]
}

// goldenCases mirrors golden_data.py: base and target files of each synthetic switch.
func goldenCases() map[string][2]map[string][]byte {
	const n = 2 << 20
	base := goldenData(1, n)
	cfg := func(b int) []byte { return []byte(fmt.Sprintf("{\"batch\": %d}\n", b)) }
	inplace := append([]byte(nil), base...)
	copy(inplace[1<<20:], goldenData(2, 40<<10))
	const blk = 32 << 10
	idx := make([]int, n/blk)
	keys := make([][32]byte, len(idx))
	for i := range idx {
		idx[i], keys[i] = i, sha256.Sum256([]byte(fmt.Sprintf("perm:%d", i)))
	}
	sort.Slice(idx, func(a, b int) bool { return bytes.Compare(keys[idx[a]][:], keys[idx[b]][:]) < 0 })
	var rearranged []byte
	for _, i := range idx {
		rearranged = append(rearranged, base[i*blk:(i+1)*blk]...)
	}
	b := map[string][]byte{"model.bin": base, "config.json": cfg(2)}
	return map[string][2]map[string][]byte{
		"identical":  {b, {"model.bin": base, "config.json": cfg(2)}},
		"inplace":    {b, {"model.bin": inplace, "config.json": cfg(8)}},
		"shifted":    {b, {"model.bin": append(goldenData(3, 17952), base...), "config.json": cfg(2)}},
		"rearranged": {b, {"model.bin": rearranged, "config.json": cfg(2)}},
	}
}

// The golden switch plans that the binding conformance kit replays are what onggi diff --ranges
// --json produces for the synthetic artifacts (ADR-0010). Manifest digests carry a creation time,
// so they are compared only for presence.
func TestGoldenSwitchPlans(t *testing.T) {
	ctx := context.Background()
	for name, c := range goldenCases() {
		s := openTwoLevel(t, t.TempDir())
		bm, nm := pushDir(t, s, c[0]), pushDir(t, s, c[1])
		st, err := s.Diff(ctx, bm, nm, DiffOptions{Ranges: true})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join("..", "..", "conformance", "binding", "golden", name+".switchplan.json")
		if *updateGolden {
			data, _ := json.MarshalIndent(st, "", "  ")
			if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var want DiffStats
		if err := json.Unmarshal(data, &want); err != nil {
			t.Fatal(err)
		}
		if want.BaseManifest == "" || want.Manifest == "" {
			t.Fatalf("%s: golden plan without manifest digests", name)
		}
		want.BaseManifest, want.Manifest = st.BaseManifest, st.Manifest
		got, _ := json.Marshal(st)
		exp, _ := json.Marshal(want)
		if !bytes.Equal(got, exp) {
			t.Errorf("%s: onggi diff no longer produces the golden switch plan; if intended, run go test ./pkg/store -run GoldenSwitchPlans -update\n got %s\nwant %s", name, got, exp)
		}
	}
}
