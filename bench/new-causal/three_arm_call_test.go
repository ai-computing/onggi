package main

import (
	"bytes"
	"context"
	"encoding/json"
	"math/rand"
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

func TestThreeArmRealPaths(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	b, err := bfs.Open(filepath.Join(root, "store"), backend.DurabilityNone)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.OpenWith(ctx, b, store.Create{Chunker: chunker.Params{Min: 16 << 10, Avg: 64 << 10, Max: 256 << 10}})
	if err != nil {
		t.Fatal(err)
	}
	a := make([]byte, 8<<20)
	rand.New(rand.NewSource(88)).Read(a)
	next := append([]byte(nil), a...)
	for i := 3 << 20; i < (3<<20)+(64<<10); i++ {
		next[i] ^= 0xa5
	}
	push := func(name string, data []byte) string {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
		md, _, err := s.Push(ctx, dir, store.PushOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return md.String()
	}
	baseName, targetName := "decode_S256_B2_lm.rbln", "decode_S256_B8_lm.rbln"
	base, target := push(baseName, a), push(targetName, next)
	for _, policy := range []string{"P", "C", "R"} {
		t.Run(policy, func(t *testing.T) {
			trial := t.TempDir()
			baseDir := filepath.Join(trial, "base")
			if err := os.Mkdir(baseDir, 0o755); err != nil {
				t.Fatal(err)
			}
			basePath := filepath.Join(baseDir, baseName)
			if err := os.WriteFile(basePath, a, 0o644); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(basePath)
			if err != nil {
				t.Fatal(err)
			}
			var out, stderr bytes.Buffer
			cacheDir := filepath.Join(trial, "cache")
			cb, err := bfs.Open(cacheDir, backend.DurabilityNone)
			if err != nil {
				t.Fatal(err)
			}
			cache, err := store.OpenWith(ctx, cb, store.Create{Chunker: s.Config.Chunker})
			if err != nil {
				t.Fatal(err)
			}
			targetDigest, err := digest.Parse(target)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Pull(ctx, targetDigest, filepath.Join(trial, "seed"), store.PullOptions{Cache: cache}); err != nil {
				t.Fatal(err)
			}
			args := []string{"--store", filepath.Join(root, "store"), "--base", base, "--base-dir", baseDir,
				"--cache", cacheDir, "--policy", policy, "--workers", "2", target, filepath.Join(trial, "output")}
			if code := runThree(args, &out, &stderr); code != 0 {
				t.Fatalf("policy %s returned %d: %s", policy, code, stderr.String())
			}
			var got struct {
				Policy     string          `json:"policy"`
				Manifest   string          `json:"manifest"`
				ClaimCalls int             `json:"claim_calls"`
				Admitted   bool            `json:"admitted"`
				Stats      store.PullStats `json:"stats"`
			}
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Policy != policy || got.Manifest != target {
				t.Fatalf("wrong treatment/identity: %+v", got)
			}
			if got.Stats.ReadBytes != 0 || got.Stats.Cached != 0 || got.Stats.PeerBytes != 0 {
				t.Fatalf("complete cache did not supply residual content: %+v", got.Stats)
			}
			targetPath := filepath.Join(trial, "output", targetName)
			data, err := os.ReadFile(targetPath)
			if err != nil || !bytes.Equal(data, next) {
				t.Fatalf("wrong full target: %v", err)
			}
			if policy == "R" {
				after, err := os.Stat(targetPath)
				if err != nil || !os.SameFile(before, after) || got.ClaimCalls != 1 || !got.Admitted || got.Stats.Base != base || got.Stats.Reused == 0 || got.Stats.LocalBytes != 0 {
					t.Fatalf("consumption not witnessed: %+v err=%v", got, err)
				}
				if _, err := os.Stat(basePath); !os.IsNotExist(err) {
					t.Fatalf("consumed original still present: %v", err)
				}
			} else {
				data, err := os.ReadFile(basePath)
				after, statErr := os.Stat(basePath)
				if err != nil || statErr != nil || !bytes.Equal(data, a) || !os.SameFile(before, after) || got.Admitted || got.Stats.Base != "" || got.Stats.Reused != 0 {
					t.Fatalf("intact donor changed: %+v read=%v stat=%v", got, err, statErr)
				}
				if policy == "C" && (got.ClaimCalls != 1 || got.Stats.LocalBase != base || got.Stats.LocalBytes == 0) {
					t.Fatalf("refused real copy not witnessed: %+v", got)
				}
				if policy == "P" && (got.ClaimCalls != 0 || got.Stats.LocalBase != "" || got.Stats.LocalBytes != 0) {
					t.Fatalf("plain arm used donor: %+v", got)
				}
			}
		})
	}
}

func TestThreeArmRejectsUnknownPolicy(t *testing.T) {
	var out, stderr bytes.Buffer
	if runThree([]string{"--policy", "unknown"}, &out, &stderr) == 0 {
		t.Fatal("unknown policy was accepted")
	}
}

func TestThreeArmRejectsOverlappingOutput(t *testing.T) {
	base := t.TempDir()
	output := filepath.Join(base, "output")
	var out, stderr bytes.Buffer
	args := []string{"--policy", "P", "--store", t.TempDir(), "--cache", t.TempDir(),
		"--base-dir", base, "--base", "sha256:" + strings.Repeat("0", 64),
		"sha256:" + strings.Repeat("0", 64), output}
	if code := runThree(args, &out, &stderr); code == 0 || !strings.Contains(stderr.String(), "paths overlap") {
		t.Fatalf("unsafe overlapping path not rejected before store access: code=%d err=%s", code, stderr.String())
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("unsafe output was created: %v", err)
	}
}
