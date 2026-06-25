package tui

import (
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/BenjaminBenetti/fleet-man/internal/version"
)

// TestRemoteIndicator covers the "wifi"-style remote-connection glyph: it must
// appear when ANY of the three remote surfaces (MCP, gRPC, webhook) is enabled —
// not just MCP (issue #199) — and stay hidden when all are off or no gateway URL
// is configured (so it never shows a misleading "connecting" red when the tunnel
// is a no-op). It is green once the shared tunnel is CONNECTED and red otherwise.
func TestRemoteIndicator(t *testing.T) {
	green := statusRunningStyle.Render("·))")
	red := errorStyle.Render("·))")
	const gw = "https://gateway.example.com"

	connected := &fleetgrpc.RemoteMcpStatus{State: fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_CONNECTED}
	connecting := &fleetgrpc.RemoteMcpStatus{State: fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_CONNECTING}
	errored := &fleetgrpc.RemoteMcpStatus{State: fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_ERROR}

	cases := []struct {
		name   string
		cfg    *state.Config
		status *fleetgrpc.RemoteMcpStatus
		want   string
	}{
		{name: "nil config hides indicator", cfg: nil, want: ""},
		{name: "all remote surfaces off hides indicator", cfg: &state.Config{}, want: ""},
		{
			name: "enabled but no gateway url hides indicator",
			cfg:  &state.Config{RemoteMcpSettings: state.RemoteMcpSettings{Enabled: true}},
			status: connecting, want: "",
		},
		{
			name: "mcp enabled, connected -> green",
			cfg:  &state.Config{RemoteMcpSettings: state.RemoteMcpSettings{Enabled: true, GatewayURL: gw}},
			status: connected, want: green,
		},
		{
			name: "grpc (fleet) enabled, connected -> green",
			cfg:  &state.Config{RemoteMcpSettings: state.RemoteMcpSettings{FleetEnabled: true, GatewayURL: gw}},
			status: connected, want: green,
		},
		{
			name: "webhook enabled, connected -> green",
			cfg:  &state.Config{RemoteMcpSettings: state.RemoteMcpSettings{WebhookEnabled: true, GatewayURL: gw}},
			status: connected, want: green,
		},
		{
			name: "multiple surfaces enabled, connected -> green",
			cfg:  &state.Config{RemoteMcpSettings: state.RemoteMcpSettings{FleetEnabled: true, WebhookEnabled: true, GatewayURL: gw}},
			status: connected, want: green,
		},
		{
			name: "enabled but connecting -> red",
			cfg:  &state.Config{RemoteMcpSettings: state.RemoteMcpSettings{WebhookEnabled: true, GatewayURL: gw}},
			status: connecting, want: red,
		},
		{
			name: "enabled but errored -> red",
			cfg:  &state.Config{RemoteMcpSettings: state.RemoteMcpSettings{Enabled: true, GatewayURL: gw}},
			status: errored, want: red,
		},
		{
			name: "enabled but no status yet -> red",
			cfg:  &state.Config{RemoteMcpSettings: state.RemoteMcpSettings{FleetEnabled: true, GatewayURL: gw}},
			status: nil, want: red,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &model{config: tc.cfg, remoteMcpStatus: tc.status}
			if got := remoteIndicator(m); got != tc.want {
				t.Fatalf("remoteIndicator = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestVersionChain covers the control-chain version header rendering: local
// (collapsed when matched), remote-server (two hops), and gateway (three hops),
// including the "?" placeholder for an old gateway that reports no version and
// the "dev" label for a versionless build.
func TestVersionChain(t *testing.T) {
	cases := []struct {
		name            string
		gateway, server string // FLEET_GATEWAY / FLEET_SERVER env
		tui, fleetd, gw string // version values
		gwStatusPresent bool   // whether a RemoteMcpStatus is set (vs nil)
		want            string
	}{
		{
			name: "local exact match collapses to tui only",
			tui:  "v1.2.3", fleetd: "v1.2.3", want: "v1.2.3",
		},
		{
			name: "local daemon not yet known collapses to tui only",
			tui:  "v1.2.3", fleetd: "", want: "v1.2.3",
		},
		{
			name: "local dev build with matching daemon renders nothing",
			tui:  "", fleetd: "", want: "",
		},
		{
			name: "local mismatch expands to tui then fleetd",
			tui:  "v1.2.3", fleetd: "v1.2.0", want: "v1.2.3 → v1.2.0",
		},
		{
			name:   "remote server (no gateway) is two hops",
			server: "host:50051",
			tui:    "v1.2.3", fleetd: "v1.2.0", want: "v1.2.3 → v1.2.0",
		},
		{
			name:    "gateway is three hops",
			gateway: "http://gw.example/abc",
			tui:     "v1.3.0", fleetd: "v1.2.0", gw: "v1.2.5", gwStatusPresent: true,
			want: "v1.3.0 → v1.2.5 → v1.2.0",
		},
		{
			name:    "gateway all three identical collapses to xN",
			gateway: "http://gw.example/abc",
			tui:     "v1.0.19-beta", fleetd: "v1.0.19-beta", gw: "v1.0.19-beta", gwStatusPresent: true,
			want: "v1.0.19-beta x3",
		},
		{
			name:    "gateway two ends match but gateway differs stays expanded",
			gateway: "http://gw.example/abc",
			tui:     "v1.0.19-beta", fleetd: "v1.0.19-beta", gw: "v1.0.18-beta", gwStatusPresent: true,
			want: "v1.0.19-beta → v1.0.18-beta → v1.0.19-beta",
		},
		{
			name:   "remote server both identical collapses to xN",
			server: "host:50051",
			tui:    "v1.0.19-beta", fleetd: "v1.0.19-beta", want: "v1.0.19-beta x2",
		},
		{
			name:    "old gateway (no version, nil status) shows ? for the unknown hop",
			gateway: "http://gw.example/abc",
			tui:     "v1.3.0", fleetd: "v1.2.0", gwStatusPresent: false,
			want: "v1.3.0 → ? → v1.2.0",
		},
		{
			name:    "gateway with dev-build daemon labels the empty hops",
			gateway: "http://gw.example/abc",
			tui:     "", fleetd: "", gw: "gw-1", gwStatusPresent: true,
			want: "dev → gw-1 → dev",
		},
	}

	orig := version.Version
	t.Cleanup(func() { version.Version = orig })

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FLEET_GATEWAY", tc.gateway)
			t.Setenv("FLEET_SERVER", tc.server)
			version.Version = tc.tui

			m := &model{serverVersion: tc.fleetd}
			if tc.gwStatusPresent {
				m.remoteMcpStatus = &fleetgrpc.RemoteMcpStatus{GatewayVersion: tc.gw}
			}

			if got := versionChain(m); got != tc.want {
				t.Fatalf("versionChain = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestJoinVersionsEdges covers joinVersions directly for the degenerate arg
// counts the chain callers never hit but which must not panic or emit "x1".
func TestJoinVersionsEdges(t *testing.T) {
	if got := joinVersions(); got != "" {
		t.Fatalf("joinVersions() = %q, want empty (and no panic)", got)
	}
	if got := joinVersions("v1"); got != "v1" {
		t.Fatalf("joinVersions(single) = %q, want bare \"v1\" (not \"v1 x1\")", got)
	}
	if got := joinVersions("v1", "v1"); got != "v1 x2" {
		t.Fatalf("joinVersions(two same) = %q, want \"v1 x2\"", got)
	}
	if got := joinVersions("v1", "v2"); got != "v1 → v2" {
		t.Fatalf("joinVersions(two diff) = %q, want \"v1 → v2\"", got)
	}
}
