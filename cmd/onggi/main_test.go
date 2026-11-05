package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSize(t *testing.T) {
	for in, want := range map[string]int{"64KiB": 64 << 10, "1MiB": 1 << 20, "2G": 2 << 30, "1000": 1000} {
		if got, err := size(in); err != nil || got != want {
			t.Errorf("size(%q) = %d, %v", in, got, err)
		}
	}
	if _, err := size("lots"); err == nil {
		t.Error("size(lots) accepted")
	}
}

func TestDedupReportRuns(t *testing.T) {
	dir := t.TempDir()
	data := bytes.Repeat([]byte("onggi "), 200_000)
	for _, name := range []string{"a.safetensors", "b.rbln"} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var out, errb bytes.Buffer
	if code := dedupReport([]string{"-quiet", "-fixed-block", "1MiB", dir}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "stored after dedup") {
		t.Fatalf("unexpected output:\n%s", out.String())
	}
	out.Reset()
	if code := dedupReport([]string{"-quiet", "-json", dir}, &out, &errb); code != 0 || !strings.Contains(out.String(), `"ratio"`) {
		t.Fatalf("json output: exit %d\n%s", code, out.String())
	}
}

func TestDedupReportClassify(t *testing.T) {
	dir, ref := t.TempDir(), t.TempDir()
	data := bytes.Repeat([]byte("onggi "), 200_000)
	if err := os.WriteFile(filepath.Join(dir, "copy.bin"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ref, "model.safetensors"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := dedupReport([]string{"-quiet", "-json", "-reference", ref, dir}, &out, &errb); code != 0 ||
		!strings.Contains(out.String(), `"class": "referenced"`) || !strings.Contains(out.String(), `"rearranged_gap": 0.25`) {
		t.Fatalf("classify json: exit %d %s\n%s", code, errb.String(), out.String())
	}
	out.Reset()
	if code := dedupReport([]string{"-quiet", "-classify", dir}, &out, &errb); code != 0 || !strings.Contains(out.String(), "placement") {
		t.Fatalf("classify table: exit %d\n%s", code, out.String())
	}
	if code := dedupReport([]string{"-quiet", "-classify", "-fine", "32KiB", dir}, &out, &errb); code != 2 {
		t.Fatalf("fine chunks larger than the chunk minimum: exit %d", code)
	}
}
