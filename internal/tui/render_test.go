package tui

import (
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/version"
)

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
