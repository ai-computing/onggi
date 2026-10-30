package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ai-computing/onggi/pkg/backend"
	bfs "github.com/ai-computing/onggi/pkg/backend/fs"
	"github.com/ai-computing/onggi/pkg/chunker"
	"github.com/ai-computing/onggi/pkg/digest"
	"github.com/ai-computing/onggi/pkg/store"
)

const prepareProfileJSON = `{"schema":"onggi.profile.v1","model_source":"sha256:1111111111111111111111111111111111111111111111111111111111111111","options":"sha256:2222222222222222222222222222222222222222222222222222222222222222","device":"rngd","runtime":"rngd-pd","sdk":"2026.4.0","compiler":"2026.4.0","firmware":"fw1","format":"runnable","layout":"rngd-bf16","topology":"fusion8-mesh2","tp":1,"batch":4,"sequence":256}`

type prepareFixture struct {
	root, source, storage, request string
	data                           []byte
}

func newPrepareFixture(t *testing.T) prepareFixture {
	t.Helper()
	root := t.TempDir()
	f := prepareFixture{root, filepath.Join(root, "source"), filepath.Join(root, "store"), filepath.Join(root, "request.json"), bytes.Repeat([]byte("native artifact bytes\n"), 10000)}
	if err := os.Mkdir(f.source, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.source, "graph.bin"), f.data, 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.request, []byte(prepareProfileJSON), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	if code := push([]string{"--store", f.storage, "--profile", f.request, "--ref", "model", "--compress", "none", "--quiet", f.source}, &out, &errs); code != 0 {
		t.Fatalf("profile publication exit %d: %s", code, &errs)
	}
	return f
}

func runPrepare(t *testing.T, args ...string) (int, map[string]any, string) {
	t.Helper()
	var out, errs bytes.Buffer
	code := prepare(args, &out, &errs)
	var report map[string]any
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("not a JSON report: %s / %s", &out, &errs)
	}
	return code, report, errs.String()
}

func TestPreparePublishesExactFilesAndReusesCache(t *testing.T) {
	f := newPrepareFixture(t)
	cache := filepath.Join(f.root, "cache")
	for i, name := range []string{"first", "second"} {
		dest := filepath.Join(f.root, name)
		code, report, errtext := runPrepare(t, "--store", f.storage, "--request", f.request, "--dir", dest, "--cache", cache, "model")
		if code != 0 || report["ready"] != true {
			t.Fatalf("not ready: %d %+v %s", code, report, errtext)
		}
		got, err := os.ReadFile(filepath.Join(dest, "graph.bin"))
		if err != nil || !bytes.Equal(got, f.data) {
			t.Fatalf("bad output: %v", err)
		}
		if i == 1 {
			stats := report["stats"].(map[string]any)
			if stats["read_bytes"] != float64(0) || stats["cache_bytes"].(float64) <= 0 {
				t.Fatalf("warm cache unused: %+v", stats)
			}
		}
		if _, err = os.Lstat(dest + ".onggi-lock"); !os.IsNotExist(err) {
			t.Fatal("owned lock leaked")
		}
	}
}

func TestPrepareNoMatchAndPlanDoNotPrepareFiles(t *testing.T) {
	for _, plan := range []bool{false, true} {
		t.Run(map[bool]string{false: "mismatch", true: "plan"}[plan], func(t *testing.T) {
			f := newPrepareFixture(t)
			dest := filepath.Join(f.root, "output")
			cache := filepath.Join(f.root, "cache")
			args := []string{"--store", f.storage, "--request", f.request, "--dir", dest, "--cache", cache}
			if plan {
				args = append(args, "--plan-only")
			} else {
				os.WriteFile(f.request, []byte(strings.Replace(prepareProfileJSON, `"sequence":256`, `"sequence":512`, 1)), 0600)
			}
			code, report, _ := runPrepare(t, append(args, "model")...)
			if report["ready"] != false || plan != (code == 0) {
				t.Fatalf("wrong readiness: %d %+v", code, report)
			}
			if !plan {
				sel := report["selection"].(map[string]any)
				if len(sel["rejected"].([]any)) != 1 {
					t.Fatal("no mismatch report")
				}
			}
			for _, p := range []string{dest, cache, dest + ".onggi-lock"} {
				if _, err := os.Lstat(p); !os.IsNotExist(err) {
					t.Fatalf("unexpected write: %s", p)
				}
			}
		})
	}
}

func TestPrepareRefusesExistingAndBusyDestinations(t *testing.T) {
	for _, kind := range []string{"directory", "file", "symlink", "busy"} {
		t.Run(kind, func(t *testing.T) {
			f := newPrepareFixture(t)
			dest := filepath.Join(f.root, "output")
			sentinel := filepath.Join(f.root, "sentinel")
			os.WriteFile(sentinel, []byte("keep"), 0600)
			switch kind {
			case "directory":
				os.Mkdir(dest, 0700)
				os.WriteFile(filepath.Join(dest, "keep"), []byte("keep"), 0600)
			case "file":
				os.WriteFile(dest, []byte("keep"), 0600)
			case "symlink":
				if err := os.Symlink(sentinel, dest); err != nil {
					t.Skip(err)
				}
			case "busy":
				os.Mkdir(dest+".onggi-lock", 0700)
				os.WriteFile(filepath.Join(dest+".onggi-lock", "owner"), []byte("other"), 0600)
			}
			code, report, _ := runPrepare(t, "--store", f.storage, "--request", f.request, "--dir", dest, "model")
			if code == 0 || report["ready"] != false {
				t.Fatal("existing/busy target accepted")
			}
			got, err := os.ReadFile(sentinel)
			if err != nil || string(got) != "keep" {
				t.Fatal("outside target damaged")
			}
			if kind == "busy" {
				got, err = os.ReadFile(filepath.Join(dest+".onggi-lock", "owner"))
				if err != nil || string(got) != "other" {
					t.Fatal("another owner lock damaged")
				}
			}
		})
	}
}

func TestPrepareCorruptPackNeverPublishesAndCleansOwnedTemp(t *testing.T) {
	f := newPrepareFixture(t)
	packs, err := filepath.Glob(filepath.Join(f.storage, "packs", "*", "*"))
	if err != nil || len(packs) == 0 {
		t.Fatal("no pack")
	}
	for _, p := range packs {
		data, _ := os.ReadFile(p)
		for i := range data {
			data[i] ^= 0xff
		}
		if err = os.WriteFile(p, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	dest := filepath.Join(f.root, "output")
	code, report, _ := runPrepare(t, "--store", f.storage, "--request", f.request, "--dir", dest, "model")
	if code == 0 || report["ready"] != false {
		t.Fatal("corrupt bytes published")
	}
	for _, p := range []string{dest, dest + ".onggi-lock"} {
		if _, err = os.Lstat(p); !os.IsNotExist(err) {
			t.Fatalf("partial state: %s", p)
		}
	}
	leftovers, _ := filepath.Glob(filepath.Join(f.root, ".output.onggi-*"))
	if len(leftovers) != 0 {
		t.Fatalf("temp leak: %v", leftovers)
	}
}

func TestPrepareInvalidRequestNeverOpensBackend(t *testing.T) {
	root := t.TempDir()
	request := filepath.Join(root, "request.json")
	storage := filepath.Join(root, "store")
	os.WriteFile(request, []byte(`{"sdk":"unknown"}`), 0600)
	code, report, _ := runPrepare(t, "--store", storage, "--request", request, "--plan-only", "model")
	if code != 2 || report["ready"] != false {
		t.Fatalf("bad request accepted: %d %+v", code, report)
	}
	if _, err := os.Stat(storage); !os.IsNotExist(err) {
		t.Fatal("backend opened on invalid request")
	}
	var out, errs bytes.Buffer
	if push([]string{"--store", storage, "--profile", request, root}, &out, &errs) != 2 {
		t.Fatal("invalid published profile accepted")
	}
	if _, err := os.Stat(storage); !os.IsNotExist(err) {
		t.Fatal("backend opened on invalid push profile")
	}
}

func TestPrepareSelectedDigestSurvivesRefChange(t *testing.T) {
	f := newPrepareFixture(t)
	ctx := context.Background()
	b, err := bfs.Open(f.storage, backend.DurabilityNone)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, b, chunker.Default, 0)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Resolve(ctx, b, "model")
	if err != nil {
		t.Fatal(err)
	}
	p, err := store.DecodeProfile(strings.NewReader(prepareProfileJSON))
	if err != nil {
		t.Fatal(err)
	}
	selection, err := s.Select(ctx, []digest.Digest{first}, p)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(f.source, "graph.bin"), []byte("different new ref"), 0640)
	second, _, err := s.Push(ctx, f.source, store.PushOptions{Profile: &p})
	if err != nil {
		t.Fatal(err)
	}
	if err = b.SetRef(ctx, "model", first.String(), second.String()); err != nil {
		t.Fatal(err)
	}
	selected, err := digest.Parse(selection.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(f.root, "selected")
	if _, err = prepareFiles(ctx, s, selected, dest, store.PullOptions{}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "graph.bin"))
	if err != nil || !bytes.Equal(got, f.data) {
		t.Fatal("mutable ref replaced selected digest")
	}
}
