package fs

import (
	"testing"

	"github.com/ai-computing/onggi/pkg/backend"
	"github.com/ai-computing/onggi/pkg/backend/backendtest"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir(), backend.DurabilityNone)
	if err != nil {
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

func TestRejectsPathTricks(t *testing.T) {
	s := open(t)
	for _, k := range []backend.Key{{Kind: backend.Pack, Name: "../x"}, {Kind: backend.Pack, Name: "a/b"}, {Kind: "../p", Name: "ab"}, {Kind: backend.Pack, Name: ""}} {
		if _, err := s.path(k); err == nil {
			t.Errorf("path(%v) accepted", k)
		}
	}
}
