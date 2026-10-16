package session

import (
	"runtime"
	"sync"
	"sync/atomic"
)

// scanWorkers bounds concurrent transcript parses during store scans, capping
// peak memory at this many simultaneous parses. A var so tests can pin it.
var scanWorkers = min(runtime.GOMAXPROCS(0), 16)

// scanEach runs fn over every path concurrently and returns fn's results in
// input order. A fixed pool of workers pulls indices, so the goroutine count
// stays flat however many paths there are. Callers that can stop early (a
// result quota) pass chunks of at most the worker count so unread files are
// never parsed.
func scanEach[T any](paths []string, fn func(path string) T) []T {
	out := make([]T, len(paths))
	workers := max(scanWorkers, 1) // a pinned zero must not stall the pool
	var next atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for {
				i := int(next.Add(1)) - 1
				if i >= len(paths) {
					return
				}
				out[i] = fn(paths[i])
			}
		})
	}
	wg.Wait()
	return out
}
