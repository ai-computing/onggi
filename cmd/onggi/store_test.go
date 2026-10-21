package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPushPullCLI(t *testing.T) {
	src, st, out := t.TempDir(), t.TempDir(), t.TempDir()
	data := bytes.Repeat([]byte("onggi push pull "), 100_000)
	if err := os.WriteFile(filepath.Join(src, "w.safetensors"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	var o, e bytes.Buffer
	if code := push([]string{"--store", st, "--ref", "m@x/y", "--quiet", src}, &o, &e); code != 0 {
		t.Fatalf("push exit %d: %s", code, e.String())
	}
	if code := pull([]string{"--store", st, "m@x/y", out}, &o, &e); code != 0 {
		t.Fatalf("pull exit %d: %s", code, e.String())
	}
	got, err := os.ReadFile(filepath.Join(out, "w.safetensors"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("pulled file differs: %v", err)
	}
	o.Reset()
	if code := ls([]string{"--store", st}, &o, &e); code != 0 || !strings.Contains(o.String(), "m@x/y") {
		t.Fatalf("ls: exit %d\n%s", code, o.String())
	}
	o.Reset()
	if code := inspect([]string{"--store", st, "m@x/y"}, &o, &e); code != 0 || !strings.Contains(o.String(), "w.safetensors") {
		t.Fatalf("inspect: exit %d\n%s", code, o.String())
	}
	o.Reset()
	if code := push([]string{"--store", st, "--ref", "m@x/y", "--quiet", src}, &o, &e); code != 0 || !strings.Contains(o.String(), "new      0 B") {
		t.Fatalf("second push should add nothing: exit %d\n%s%s", code, o.String(), e.String())
	}
}
