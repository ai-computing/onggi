package csi

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ai-computing/onggi/pkg/digest"
	"github.com/ai-computing/onggi/pkg/store"
)

func TestEnsureBoundsDistinctPulls(t *testing.T) {
	for _, limit := range []int{0, 1, 2} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) { checkPullLimit(t, limit) })
	}
}

func checkPullLimit(t *testing.T, limit int) {
	t.Helper()
	entered := make(chan digest.Digest, 3)
	gate := make(chan struct{})
	m := &Materializer{Root: t.TempDir(), MaxPulls: limit, Pull: func(_ context.Context, d digest.Digest, _ string, _ *Offer) (store.PullStats, error) {
		entered <- d
		<-gate
		return store.PullStats{}, nil
	}}
	if err := m.Init(); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	defer func() { close(gate); wg.Wait() }()
	for i := byte(0); i < 3; i++ {
		wg.Add(1)
		go func(i byte) {
			defer wg.Done()
			if _, err := m.Ensure(context.Background(), digest.Of([]byte{i})); err != nil {
				t.Error(err)
			}
		}(i)
	}
	for i := 0; i < max(1, limit); i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("configured number of pulls did not start")
		}
	}
	select {
	case <-entered:
		t.Fatal("multiple distinct pulls run without a concurrency budget")
	case <-time.After(100 * time.Millisecond):
	}
}
