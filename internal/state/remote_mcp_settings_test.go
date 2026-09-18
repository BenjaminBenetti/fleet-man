package state

import "testing"

// TestTunnelDesired pins the "tunnel should be up" predicate: any of the three
// gateway surfaces enabled AND a (non-blank) gateway URL set. It must mirror the
// manager's desiredState.on() so the TUI indicator and the actual dial agree.
// Remote fleet in SSH mode is NOT a gateway surface.
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
		{name: "grpc on + explicit gateway mode + url", s: RemoteMcpSettings{FleetEnabled: true, FleetMode: FleetModeGateway, GatewayURL: "https://gw"}, want: true},
		{name: "grpc on in ssh mode + url: no tunnel", s: RemoteMcpSettings{FleetEnabled: true, FleetMode: FleetModeSSH, GatewayURL: "https://gw"}, want: false},
		{name: "ssh mode grpc + mcp on + url: tunnel for mcp", s: RemoteMcpSettings{Enabled: true, FleetEnabled: true, FleetMode: FleetModeSSH, GatewayURL: "https://gw"}, want: true},
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

// TestFleetModePredicates pins the SSH/gateway split: exactly one of the two
// predicates holds while FleetEnabled is on, neither while it is off, and an
// unknown mode value falls back to gateway.
func TestFleetModePredicates(t *testing.T) {
	cases := []struct {
		name        string
		s           RemoteMcpSettings
		ssh, gatewy bool
	}{
		{name: "off", s: RemoteMcpSettings{}, ssh: false, gatewy: false},
		{name: "off in ssh mode", s: RemoteMcpSettings{FleetMode: FleetModeSSH}, ssh: false, gatewy: false},
		{name: "on, default mode", s: RemoteMcpSettings{FleetEnabled: true}, ssh: false, gatewy: true},
		{name: "on, gateway", s: RemoteMcpSettings{FleetEnabled: true, FleetMode: FleetModeGateway}, ssh: false, gatewy: true},
		{name: "on, ssh", s: RemoteMcpSettings{FleetEnabled: true, FleetMode: FleetModeSSH}, ssh: true, gatewy: false},
		{name: "on, unknown mode is gateway", s: RemoteMcpSettings{FleetEnabled: true, FleetMode: "carrier-pigeon"}, ssh: false, gatewy: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.FleetViaSSH(); got != tc.ssh {
				t.Errorf("FleetViaSSH = %v, want %v", got, tc.ssh)
			}
			if got := tc.s.FleetViaGateway(); got != tc.gatewy {
				t.Errorf("FleetViaGateway = %v, want %v", got, tc.gatewy)
			}
		})
	}
}
