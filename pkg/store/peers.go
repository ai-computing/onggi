package store

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"os"
	"sync"
	"sync/atomic"

	"github.com/ai-computing/onggi/pkg/codec"
	"github.com/ai-computing/onggi/pkg/digest"
)

// Peer reads (#34): after the node cache, the stored chunks a pull still needs are asked from
// the peers. Each chunk goes to one holder, the one with the highest rendezvous score
// hash(chunk, peer) among the peers that hold it (ADR-0003), so load spreads the same way on
// every node. A chunk a peer does not deliver, or delivers corrupt, is read from the store.

const (
	peerHasBatch   = 1 << 16 // digests per Has call
	peerFetchBatch = 4096    // chunks per Fetch call
)

func hrw(d digest.Digest, peer string) uint64 {
	h := sha256.New()
	h.Write(d[:])
	h.Write([]byte(peer))
	return binary.BigEndian.Uint64(h.Sum(nil))
}

// fromPeers writes the chunks peers deliver and marks them done; it returns the stored bytes
// received. Peer failures are not errors: the store serves what peers do not.
func (s *Store) fromPeers(ctx context.Context, peers []ChunkSource, workers int, nodes []node, done []bool,
	handles []*os.File, fill *packWriter) (int64, int64) {
	var need []int32
	for i := range nodes {
		if nodes[i].stored && !nodes[i].fromCache && !done[i] {
			need = append(need, int32(i))
		}
	}
	if len(need) == 0 {
		return 0, 0
	}
	ds := make([]digest.Digest, len(need))
	for k, i := range need {
		ds[k] = nodes[i].chunk
	}
	holder := make([]int, len(need))
	score := make([]uint64, len(need))
	for k := range holder {
		holder[k] = -1
	}
	for pi, p := range peers {
		for a := 0; a < len(ds); a += peerHasBatch {
			b := min(a+peerHasBatch, len(ds))
			has, err := p.Has(ctx, ds[a:b])
			if err != nil || len(has) != b-a {
				break // this peer serves nothing in this pull
			}
			for k, h := range has {
				if !h {
					continue
				}
				if sc := hrw(ds[a+k], p.Name()); holder[a+k] < 0 || sc > score[a+k] {
					holder[a+k], score[a+k] = pi, sc
				}
			}
		}
	}
	type batch struct {
		peer int
		ks   []int // positions in need
	}
	var batches []batch
	per := make([][]int, len(peers))
	for k, h := range holder {
		if h >= 0 {
			per[h] = append(per[h], k)
		}
	}
	for pi, ks := range per {
		for a := 0; a < len(ks); a += peerFetchBatch {
			batches = append(batches, batch{pi, ks[a:min(a+peerFetchBatch, len(ks))]})
		}
	}
	if len(batches) == 0 {
		return 0, 0
	}
	byDigest := make(map[digest.Digest]int32, len(need))
	for _, i := range need {
		byDigest[nodes[i].chunk] = i
	}
	var bytes, chunks atomic.Int64
	jobs := make(chan batch)
	var wg sync.WaitGroup
	for w := 0; w < max(workers, 1); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for bt := range jobs {
				want := make([]digest.Digest, len(bt.ks))
				for j, k := range bt.ks {
					want[j] = ds[k]
				}
				peers[bt.peer].Fetch(ctx, want, func(d digest.Digest, stored []byte, id codec.ID, rawLen uint32) error {
					i, ok := byDigest[d]
					if !ok || done[i] || rawLen != nodes[i].loc.Length {
						return nil
					}
					raw, err := codec.Decode(stored, id, int(rawLen))
					if err != nil || digest.Of(raw) != d {
						return nil // corrupt or undecodable: the store serves it
					}
					if err := emit(nodes, i, raw, 0, handles); err != nil {
						return err
					}
					if fill != nil {
						if err := fill.add(ctx, d, stored, id, rawLen); err != nil {
							return err
						}
					}
					done[i] = true
					bytes.Add(int64(len(stored)))
					chunks.Add(1)
					return nil
				})
			}
		}()
	}
	for _, bt := range batches {
		jobs <- bt
	}
	close(jobs)
	wg.Wait()
	return bytes.Load(), chunks.Load()
}
