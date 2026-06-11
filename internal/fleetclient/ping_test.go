package fleetclient

import (
	"context"
	"testing"
)

// TestPingRejectsMalformedURL verifies Ping validates the gateway URL shape
// before touching the network (same rules as FLEET_GATEWAY parsing).
func TestPingRejectsMalformedURL(t *testing.T) {
	cases := []string{
		"ftp://gw.example.com/abc", // bad scheme
		"https:///abc",             // no host
		"https://gw.example.com",   // no session id
	}
	for _, url := range cases {
		if _, err := Ping(context.Background(), url, "tok"); err == nil {
			t.Fatalf("Ping(%q) should fail validation", url)
		}
	}
}
