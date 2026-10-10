package codespaces

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// TestNameCacheIsSafeAcrossGoroutines: the server's pollers share one backend
// per instance across goroutines, so registering names (and the SSH configs
// that come with them) races resolving them unless the caches are locked. Run
// under -race.
func TestNameCacheIsSafeAcrossGoroutines(t *testing.T) {
	b := New()
	root := t.TempDir()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		ws := filepath.Join(root, fmt.Sprintf("i%d", i), "ws")
		name := fmt.Sprintf("cs-%d", i)
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				b.RegisterName(ws, name)
				b.cacheSSHConfig(name, sshConfig{configPath: ws, host: name})
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = b.resolveCodespaceName(ws)
				_ = b.sshCommand(name, []string{"true"}, false)
			}
		}()
	}
	wg.Wait()
	if got := b.resolveCodespaceName(filepath.Join(root, "i3", "ws")); got != "cs-3" {
		t.Fatalf("resolveCodespaceName = %q, want cs-3", got)
	}
}
