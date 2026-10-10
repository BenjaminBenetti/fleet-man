package coder

import (
	"fmt"
	"sync"
	"testing"
)

// TestNameCacheIsSafeAcrossGoroutines: the server's pollers share one backend
// per instance across goroutines, so registering names races resolving them
// unless the cache is locked. Run under -race.
func TestNameCacheIsSafeAcrossGoroutines(t *testing.T) {
	b := New()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		ws := fmt.Sprintf("/ws/f/i%d/f", i)
		name := fmt.Sprintf("ws-%d.dev", i)
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				b.RegisterName(ws, name)
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = b.resolveWorkspaceName(ws)
			}
		}()
	}
	wg.Wait()
	if got := b.resolveWorkspaceName("/ws/f/i3/f"); got != "ws-3.dev" {
		t.Fatalf("resolveWorkspaceName = %q, want ws-3.dev", got)
	}
}
