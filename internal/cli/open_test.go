package cli

import (
	"strings"
	"testing"
)

// TestRequireOpenEndpoints enforces open's shape: an instance (or self) source,
// and an empty or host-side destination — the only place a file can be opened.
func TestRequireOpenEndpoints(t *testing.T) {
	for _, src := range []string{"alpha:out/chart.png", ":build/out", "myfleet/beta:/p"} {
		for _, dst := range []string{"", "host:~/Pictures/", "host:/tmp/x", "./here/", "/abs/target"} {
			if err := requireOpenEndpoints(src, dst); err != nil {
				t.Errorf("open(%q, %q) should be allowed, got %v", src, dst, err)
			}
		}
	}
	// A source already on your machine has nothing to fetch.
	for _, src := range []string{"./file", "plain.txt", "host:/tmp/x", "/abs/file"} {
		err := requireOpenEndpoints(src, "")
		if err == nil || !strings.Contains(err.Error(), "source must be in an instance") {
			t.Errorf("open(%q) should reject an own-machine source, got %v", src, err)
		}
	}
	// A destination inside an instance can't be opened on your machine. In the
	// in-instance form a plain dst arrives here as a `:` self endpoint, so the
	// error must steer to `host:`.
	for _, dst := range []string{":/tmp/", "alpha:/tmp/", "other/beta:x"} {
		err := requireOpenEndpoints("alpha:chart.png", dst)
		if err == nil || !strings.Contains(err.Error(), "host:") {
			t.Errorf("open(alpha:chart.png, %q) should reject an instance destination, got %v", dst, err)
		}
	}
}
