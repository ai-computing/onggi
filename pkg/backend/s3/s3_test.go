package s3

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ai-computing/onggi/pkg/backend"
	"github.com/ai-computing/onggi/pkg/backend/backendtest"
)

// The suite needs a server: set ONGGI_TEST_S3=s3://bucket?endpoint=host:port&insecure=1 and
// AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY. CI runs it against VersityGW and SeaweedFS.

var run atomic.Int64

func open(t *testing.T) *Store {
	t.Helper()
	raw := FromEnv()
	if raw == "" {
		t.Skip("ONGGI_TEST_S3 not set")
	}
	// a fresh prefix per test keeps tests independent on one bucket
	sep := "?"
	base := raw
	if i := strings.Index(raw, "?"); i >= 0 {
		base, sep = raw[:i], raw[i:]
	} else {
		sep = ""
	}
	u := fmt.Sprintf("%s/t%d-%d%s", strings.TrimRight(base, "/"), run.Add(1), testRunID, sep)
	s, err := Open(u, backend.DurabilityUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureBucket(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestConformance(t *testing.T) {
	backendtest.Run(t, func(t *testing.T) backend.Backend { return open(t) })
}

func TestRefs(t *testing.T) {
	backendtest.RunRefs(t, func(t *testing.T) backend.Refs { return open(t) })
}

func TestOpenParsesURLs(t *testing.T) {
	s, err := Open("s3://models/onggi/lab?endpoint=192.0.2.10:9000&insecure=1", backend.DurabilityErasure)
	if err != nil {
		t.Fatal(err)
	}
	if s.bucket != "models" || s.prefix != "onggi/lab" || s.Caps().Durability != backend.DurabilityErasure {
		t.Fatalf("parsed %+v", s)
	}
	k, _ := s.key(backend.Key{Kind: backend.Pack, Name: "abcdef"})
	if k != "onggi/lab/packs/ab/abcdef" {
		t.Fatalf("key = %s", k)
	}
	for _, bad := range []string{"http://x", "s3://", "models"} {
		if _, err := Open(bad, ""); err == nil {
			t.Errorf("Open(%q) accepted", bad)
		}
	}
}

var testRunID = rand.Int63()
