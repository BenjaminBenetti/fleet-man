package server

import (
	"sync"
	"sync/atomic"
)

// pollerWorkers bounds how many instances one runtime poller pass probes at
// once (sessionsPass and the capture loop of statsActivityPass). The passes
// used to walk instances one at a time, so each probe waited for every probe
// before it and one slow instance (an SSH round trip to a cloud workspace, a
// wedged container) stretched the whole pass past its tick. Unbounded fan-out
// would instead start one exec per running instance at the same moment.
const pollerWorkers = 8

// forEachBounded calls fn(i) for every i in [0, n) on at most limit goroutines
// and returns once every call has returned. fn runs concurrently with itself,
// so callers write each result into its own slot (results[i]) and merge after.
func forEachBounded(n, limit int, fn func(i int)) {
	workers := min(n, limit)
	if workers <= 1 {
		for i := range n {
			fn(i)
		}
		return
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= n {
					return
				}
				fn(i)
			}
		}()
	}
	wg.Wait()
}
