package main

import (
	"os"
	"runtime"
	"runtime/pprof"
	"time"
)

// startMemProfile samples the heap once a second when ONGGI_MEMPROFILE names a file, and rewrites
// a heap profile there each time the heap in use grows by 256 MB past its previous peak, so the
// file ends up describing the peak. It returns a function that stops sampling and reports the
// peak heap in use, in bytes (0 when profiling is off).
// With ONGGI_CPUPROFILE naming a file, it also writes a CPU profile there (#42).
func startMemProfile() func() uint64 {
	if cpu := os.Getenv("ONGGI_CPUPROFILE"); cpu != "" {
		if f, err := os.Create(cpu); err == nil && pprof.StartCPUProfile(f) == nil {
			stopMem := startHeapSampler()
			return func() uint64 {
				pprof.StopCPUProfile()
				f.Close()
				return stopMem()
			}
		}
	}
	return startHeapSampler()
}

func startHeapSampler() func() uint64 {
	path := os.Getenv("ONGGI_MEMPROFILE")
	if path == "" {
		return func() uint64 { return 0 }
	}
	stop := make(chan struct{})
	done := make(chan uint64)
	go func() {
		var peak, written uint64
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			if ms.HeapInuse > peak {
				peak = ms.HeapInuse
			}
			if peak > written+256<<20 {
				if f, err := os.Create(path); err == nil {
					pprof.WriteHeapProfile(f)
					f.Close()
					written = peak
				}
			}
			select {
			case <-stop:
				done <- peak
				return
			case <-t.C:
			}
		}
	}()
	return func() uint64 {
		close(stop)
		return <-done
	}
}
