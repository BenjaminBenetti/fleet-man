package state

import "strings"

// RemoteMcpSettings holds the user's intent to expose this daemon's local MCP
// server (Enabled) and/or its gRPC control surface (FleetEnabled) to the
// internet through a remote fleet gateway. Both ride the SAME outbound tunnel to
// GatewayURL; the two toggles are independent, so a user can expose MCP, remote
// control, or both. The local MCP server itself (internal/server/mcp.go) is
// unchanged and stays loopback-only; the gateway tunnel dials OUT to GatewayURL
// and reverse-proxies inbound requests back to it.
//
// Remote fleet control has a second transport: FleetMode == FleetModeSSH skips
// the gateway entirely and instead serves the token-gated gRPC server on a
// loopback TCP port that a remote client reaches over an SSH tunnel (see
// internal/server/sshlisten.go and internal/server/sshtunnel). Only the remote
// fleet surface has this mode — MCP and webhooks stay gateway-only.
//
// Only these fields are persisted here. The gateway-assigned "Public MCP URL" /
// "Public GRPC URL" are deliberately NOT stored: they are runtime state the
// server computes after it connects and pushes to the TUI over the Watch stream
// (fleetgrpc.RemoteMcpStatus). Keeping them out of Config is what stops
// SetConfig — which replaces the whole Config — from clobbering them.
type RemoteMcpSettings struct {
	// Enabled turns the remote-MCP gateway tunnel on. Plain bool (default
	// off), matching the create-time *Mount flags: false == "never set" ==
	// disabled.
	Enabled bool `json:"enabled,omitempty"`

	// GatewayURL is the fleet gateway to register with (e.g.
	// "https://gateway.example.com", or "http://gateway.example.com" when the
	// gateway is fronted by a TLS-terminating proxy). Empty while Enabled means
	// "enabled but not yet configured"; the tunnel manager treats that as a no-op.
	GatewayURL string `json:"gateway_url,omitempty"`

	// FleetEnabled ("Enable Remote Fleet") exposes the daemon's gRPC server so a
	// remote `fleet` binary can control this instance — through the gateway
	// (FleetMode gateway, the default) or over an SSH tunnel (FleetMode ssh).
	// When off, fleetd neither negotiates the grpc tunnel feature nor opens the
	// SSH loopback listener.
	FleetEnabled bool `json:"fleet_enabled,omitempty"`

	// WebhookEnabled ("Enable Webhook") exposes this daemon's automation webhook
	// endpoint through the gateway so a remote system can POST to
	// <public-url>/webhook/<name> and fire a matching webhook trigger's agents.
	// When off, fleetd does not negotiate the webhook tunnel feature, so the
	// gateway does not serve the webhook route for this daemon. Independent of
	// Enabled/FleetEnabled — all three ride the SAME outbound tunnel.
	WebhookEnabled bool `json:"webhook_enabled,omitempty"`

	// FleetMode selects HOW remote fleet control is reached: FleetModeGateway
	// (the default; "" reads as gateway) rides the gateway tunnel, FleetModeSSH
	// serves the token-gated gRPC server on a loopback port for an SSH tunnel.
	// Stored explicitly rather than inferred from an empty GatewayURL, so an
	// existing "enabled but unconfigured" gateway config keeps its documented
	// no-op behaviour instead of silently opening a listener.
	FleetMode string `json:"fleet_mode,omitempty"`
}

// The FleetMode values. An unknown value is treated as gateway (the default), so
// a config written by a newer daemon never flips an older one into SSH mode.
const (
	FleetModeGateway = "gateway"
	FleetModeSSH     = "ssh"
)

// FleetViaSSH reports whether remote fleet control is enabled AND set to the SSH
// transport — the predicate the SSH loopback listener converges on.
func (s RemoteMcpSettings) FleetViaSSH() bool {
	return s.FleetEnabled && s.FleetMode == FleetModeSSH
}

// FleetViaGateway reports whether remote fleet control is enabled AND set to the
// gateway transport — the predicate that makes the tunnel negotiate the grpc
// feature. Exactly one of FleetViaSSH / FleetViaGateway is true while
// FleetEnabled is on.
func (s RemoteMcpSettings) FleetViaGateway() bool {
	return s.FleetEnabled && !s.FleetViaSSH()
}

// TunnelDesired reports whether the outbound gateway tunnel should be up: any of
// the three gateway surfaces (MCP / gateway-mode gRPC / webhook) is enabled AND
// a gateway URL is configured. It is the single source of truth for that
// predicate — the manager's desiredState.on() (internal/server/remote/manager.go)
// delegates here for the dial decision, and the TUI's remote-connection
// indicator uses it to light exactly when the tunnel will actually dial. Without
// a gateway URL the tunnel is a documented no-op, so the indicator stays dark
// rather than showing a misleading "connecting" state forever. Remote fleet in
// SSH mode does not want the tunnel at all.
func (s RemoteMcpSettings) TunnelDesired() bool {
	return (s.Enabled || s.FleetViaGateway() || s.WebhookEnabled) && strings.TrimSpace(s.GatewayURL) != ""
}
