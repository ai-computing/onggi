//go:build linux

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/ai-computing/onggi/pkg/backend"
	bfs "github.com/ai-computing/onggi/pkg/backend/fs"
	"github.com/ai-computing/onggi/pkg/chunker"
	"github.com/ai-computing/onggi/pkg/store"
)

type sequenceFixture struct {
	config map[string]any
	data   map[string][]byte
	refs   map[string]string
}

func newSequenceFixture(t *testing.T, policy string) sequenceFixture {
	t.Helper()
	parent := t.TempDir()
	origin := filepath.Join(parent, "origin")
	b, err := bfs.Open(origin, backend.DurabilityNone)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.OpenWith(context.Background(), b, store.Create{
		Chunker: chunker.Params{Min: 16 << 10, Avg: 64 << 10, Max: 256 << 10}, PackSize: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	a := make([]byte, 8<<20)
	rand.New(rand.NewSource(93)).Read(a)
	bBytes := append([]byte(nil), a...)
	for i := 3 << 20; i < (3<<20)+(64<<10); i++ {
		bBytes[i] ^= 0xa5
	}
	data := map[string][]byte{"A": a, "B": bBytes}
	names := map[string]string{"A": "decode_S256_B2_lm.rbln", "B": "decode_S256_B8_lm.rbln"}
	artifacts := map[string]any{}
	refs := map[string]string{}
	for _, key := range []string{"A", "B"} {
		source := t.TempDir()
		if err := os.WriteFile(filepath.Join(source, names[key]), data[key], 0o644); err != nil {
			t.Fatal(err)
		}
		md, _, err := s.Push(context.Background(), source, store.PushOptions{})
		if err != nil {
			t.Fatal(err)
		}
		refs[key] = md.String()
		digest := sha256.Sum256(data[key])
		artifacts[key] = map[string]any{"manifest": md.String(), "files": []any{
			map[string]any{"path": names[key], "bytes": len(data[key]), "sha256": hex.EncodeToString(digest[:])},
		}}
	}
	return sequenceFixture{config: map[string]any{
		"status": "not_run", "kind": "fixture", "policy": policy, "store": origin,
		"root": filepath.Join(parent, "attempt"), "artifacts": artifacts,
		"requests": []string{"A", "B", "A"}, "workers": 2, "idle_ns": 1_000_000,
		"step_timeout_ns": int64(600_000_000_000), "sequence_timeout_ns": int64(1_800_000_000_000),
		"materialized_capacity_bytes": 32 << 20, "workspace_capacity_bytes": 32 << 20,
		"cache_capacity_bytes": 256 << 20,
	}, data: data, refs: refs}
}

func callSequence(t *testing.T, cfg map[string]any) (int, map[string]any, string) {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	code := runSequence([]string{"--config", p}, &out, &stderr)
	var report map[string]any
	if out.Len() > 0 {
		if err := json.Unmarshal(out.Bytes(), &report); err != nil {
			t.Fatalf("invalid execution record: %v: %s", err, out.String())
		}
	}
	return code, report, stderr.String()
}

func TestSequencePreservesRealIntermediateState(t *testing.T) {
	for _, policy := range []string{"P", "C", "R"} {
		t.Run(policy, func(t *testing.T) {
			f := newSequenceFixture(t, policy)
			code, report, stderr := callSequence(t, f.config)
			if code != 0 {
				t.Fatalf("policy %s returned %d: %s", policy, code, stderr)
			}
			if report["status"] != "succeeded" || report["scientific_acceptance"] != "external cohort admission required" {
				t.Fatalf("wrong execution/scope: %+v", report)
			}
			steps := report["steps"].([]any)
			if len(steps) != 3 {
				t.Fatalf("expected the complete initial A, B, A trajectory: %+v", steps)
			}
			first, second, last := steps[0].(map[string]any), steps[1].(map[string]any), steps[2].(map[string]any)
			for _, entry := range steps {
				step := entry.(map[string]any)
				if step["status"] != "succeeded" || step["target_sha_equal"] != true {
					t.Fatalf("target not fully verified: %+v", step)
				}
				if step["hash_finish_ns"].(float64) < step["ensure_finish_ns"].(float64) {
					t.Fatalf("endpoint order: %+v", step)
				}
			}
			if len(first["pulls"].([]any)) != 1 || len(second["pulls"].([]any)) != 1 {
				t.Fatal("initial A/B were not actually materialized")
			}
			pull := second["pulls"].([]any)[0].(map[string]any)
			stats := pull["stats"].(map[string]any)
			if policy == "R" {
				if last["already_present"] != false || len(last["pulls"].([]any)) != 1 ||
					pull["admitted"] != true || stats["base"] != f.refs["A"] || stats["reused"].(float64) <= 0 {
					t.Fatalf("R did not carry consumed state into the actual return: %+v", report)
				}
			} else {
				a, _ := json.Marshal(first["file_identities"])
				b, _ := json.Marshal(last["file_identities"])
				if last["already_present"] != true || len(last["pulls"].([]any)) != 0 || !bytes.Equal(a, b) {
					t.Fatalf("retained A was not credited: %+v", report)
				}
				if policy == "C" && (pull["claim_calls"] != float64(1) || pull["admitted"] != false || stats["local_bytes"].(float64) <= 0) {
					t.Fatalf("C did not use an explicit refusal and actual copy: %+v", pull)
				}
				if policy == "P" && (pull["claim_calls"] != float64(0) || stats["local_bytes"] != float64(0)) {
					t.Fatalf("P reused a donor: %+v", pull)
				}
			}
			if report["sequence_start_ns"] != second["request_ns"] || report["sequence_finish_ns"] != last["hash_finish_ns"] ||
				report["sequence_elapsed_ns"].(float64) != last["hash_finish_ns"].(float64)-second["request_ns"].(float64) {
				t.Fatalf("whole sequence is not one clock: %+v", report)
			}
			root := f.config["root"].(string)
			path := filepath.Join(root, "owner", "artifacts", f.refs["A"][7:], "decode_S256_B2_lm.rbln")
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, f.data["A"]) {
				t.Fatalf("final target bytes differ: %v", err)
			}
		})
	}
}

func TestSequenceRejectsInvalidConfigurationWithoutMutation(t *testing.T) {
	for _, name := range []string{"policy", "capacity", "timeout", "path"} {
		t.Run(name, func(t *testing.T) {
			f := newSequenceFixture(t, "R")
			switch name {
			case "policy":
				f.config["policy"] = "unknown"
			case "capacity":
				f.config["materialized_capacity_bytes"] = 1
			case "timeout":
				f.config["step_timeout_ns"] = 0
			case "path":
				f.config["root"] = filepath.Join(f.config["store"].(string), "owned")
			}
			code, _, _ := callSequence(t, f.config)
			if code != 2 {
				t.Fatalf("invalid %s got %d instead of configuration refusal", name, code)
			}
			if _, err := os.Lstat(f.config["root"].(string)); !os.IsNotExist(err) {
				t.Fatalf("refusal created output: %v", err)
			}
		})
	}
}

func TestSequenceExistingRootNeverOverwritten(t *testing.T) {
	f := newSequenceFixture(t, "R")
	root := f.config["root"].(string)
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sentinel"), []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, _ := callSequence(t, f.config)
	got, err := os.ReadFile(filepath.Join(root, "sentinel"))
	if code != 2 || err != nil || string(got) != "preserve" {
		t.Fatalf("existing state changed: code=%d err=%v got=%s", code, err, got)
	}
}

func TestSequenceBadTargetHashStopsAndRetainsAttempt(t *testing.T) {
	f := newSequenceFixture(t, "R")
	f.config["artifacts"].(map[string]any)["B"].(map[string]any)["files"].([]any)[0].(map[string]any)["sha256"] = string(bytes.Repeat([]byte{'0'}, 64))
	code, report, _ := callSequence(t, f.config)
	if code != 1 || report["status"] != "failed" || len(report["steps"].([]any)) != 2 {
		t.Fatalf("failed B was replaced or return continued: code=%d report=%+v", code, report)
	}
	if _, err := os.Stat(filepath.Join(f.config["root"].(string), "result.json")); err != nil {
		t.Fatalf("failure record missing: %v", err)
	}
}
