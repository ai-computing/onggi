package store

import (
	"cmp"
	"context"
	"os"
	"slices"
	"sync"
)

// rjob is one chunk to read from a file: n bytes at off.
type rjob struct {
	node int32
	file int
	off  int64
	n    int
}

// readSpans reads the jobs from files, joining chunks that follow one another in a file into
// single reads of at most maxRange bytes, and calls fn with each chunk's bytes. A read of 15 GB
// in 64 KiB chunks is 230 000 system calls one by one and under a thousand joined. fn gets nil
// for a chunk whose read failed. It is called from several goroutines; b is only valid during
// the call.
func readSpans(ctx context.Context, files []*os.File, jobs []rjob, workers int, maxRange int64, fn func(j rjob, b []byte)) {
	if len(jobs) == 0 {
		return
	}
	if maxRange <= 0 {
		maxRange = 16 << 20
	}
	slices.SortFunc(jobs, func(a, b rjob) int { // each file front to back
		if a.file != b.file {
			return a.file - b.file
		}
		return cmp.Compare(a.off, b.off)
	})
	type span struct{ a, b int } // jobs[a:b]
	var spans []span
	for k, j := range jobs {
		if n := len(spans); n > 0 {
			s := &spans[n-1]
			first, prev := jobs[s.a], jobs[s.b-1]
			if prev.file == j.file && prev.off+int64(prev.n) == j.off && j.off+int64(j.n)-first.off <= maxRange {
				s.b = k + 1
				continue
			}
		}
		spans = append(spans, span{k, k + 1})
	}
	ch := make(chan span)
	var wg sync.WaitGroup
	for w := 0; w < max(workers, 1); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var buf []byte
			for s := range ch {
				if ctx.Err() != nil {
					continue
				}
				first, last := jobs[s.a], jobs[s.b-1]
				size := int(last.off + int64(last.n) - first.off)
				if cap(buf) < size {
					buf = make([]byte, size)
				}
				b := buf[:size]
				_, err := files[first.file].ReadAt(b, first.off)
				for _, j := range jobs[s.a:s.b] {
					if err != nil {
						fn(j, nil)
						continue
					}
					o := int(j.off - first.off)
					fn(j, b[o:o+j.n])
				}
			}
		}()
	}
	for _, s := range spans {
		ch <- s
	}
	close(ch)
	wg.Wait()
}

// places calls fn for every place chunk id goes: a file and the offset in it.
func places(nodes []node, id int32, base int64, fn func(target)) {
	nd := &nodes[id]
	if nd.hasT {
		fn(target{nd.firstT.file, nd.firstT.off + base})
		for _, t := range nd.moreT {
			fn(target{t.file, t.off + base})
		}
	}
	if nd.hasP {
		places(nodes, nd.firstP.node, base+int64(nd.firstP.off), fn)
		for _, e := range nd.moreP {
			places(nodes, e.node, base+int64(e.off), fn)
		}
	}
}
