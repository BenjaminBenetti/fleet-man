package podwire

import (
	"testing"
	"time"
)

// The bulk session tolerates a slow link for as long as the gateway's
// registry connection does: a full window queued on a lossy WAN must not
// time its keepalive out. The agent session keeps yamux's 10s, so a dead
// pod is noticed as soon as before.
func TestSessionConfigOfEachRole(t *testing.T) {
	for _, tc := range []struct {
		role    Role
		timeout time.Duration
		window  uint32
	}{
		{RoleMain, 10 * time.Second, 256 << 10},
		{RoleBulk, 60 * time.Second, BulkWindow},
	} {
		c := SessionConfig(tc.role)
		if c.ConnectionWriteTimeout != tc.timeout || c.MaxStreamWindowSize != tc.window || c.KeepAliveInterval != KeepAliveInterval {
			t.Errorf("%s: write timeout %v, window %d, keepalive %v; want %v, %d, %v",
				tc.role, c.ConnectionWriteTimeout, c.MaxStreamWindowSize, c.KeepAliveInterval, tc.timeout, tc.window, KeepAliveInterval)
		}
	}
}
