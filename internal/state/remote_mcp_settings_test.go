package state

import "testing"

// TestTunnelDesired pins the "tunnel should be up" predicate: any of the three
// remote surfaces enabled AND a (non-blank) gateway URL set. It must mirror the
// manager's desiredState.on() so the TUI indicator and the actual dial agree.
func TestTunnelDesired(t *testing.T) {
	cases := []struct {
		name string
		s    RemoteMcpSettings
		want bool
	}{
		{name: "all off", s: RemoteMcpSettings{}, want: false},
		{name: "mcp on, no url", s: RemoteMcpSettings{Enabled: true}, want: false},
		{name: "grpc on, no url", s: RemoteMcpSettings{FleetEnabled: true}, want: false},
		{name: "webhook on, no url", s: RemoteMcpSettings{WebhookEnabled: true}, want: false},
		{name: "url set but all surfaces off", s: RemoteMcpSettings{GatewayURL: "https://gw"}, want: false},
		{name: "blank url counts as unset", s: RemoteMcpSettings{Enabled: true, GatewayURL: "   "}, want: false},
		{name: "mcp on + url", s: RemoteMcpSettings{Enabled: true, GatewayURL: "https://gw"}, want: true},
		{name: "grpc on + url", s: RemoteMcpSettings{FleetEnabled: true, GatewayURL: "https://gw"}, want: true},
		{name: "webhook on + url", s: RemoteMcpSettings{WebhookEnabled: true, GatewayURL: "https://gw"}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.TunnelDesired(); got != tc.want {
				t.Fatalf("TunnelDesired(%+v) = %v, want %v", tc.s, got, tc.want)
			}
		})
	}
}
