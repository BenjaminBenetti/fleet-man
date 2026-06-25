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

	// FleetEnabled ("Enable Remote Fleet") exposes the daemon's gRPC server
	// through the gateway so a remote `fleet` binary can control this instance.
	// When off, fleetd does not negotiate the grpc tunnel feature, so the
	// gateway rejects any incoming gRPC commands aimed at this daemon.
	FleetEnabled bool `json:"fleet_enabled,omitempty"`

	// WebhookEnabled ("Enable Webhook") exposes this daemon's automation webhook
	// endpoint through the gateway so a remote system can POST to
	// <public-url>/webhook/<name> and fire a matching webhook trigger's agents.
	// When off, fleetd does not negotiate the webhook tunnel feature, so the
	// gateway does not serve the webhook route for this daemon. Independent of
	// Enabled/FleetEnabled — all three ride the SAME outbound tunnel.
	WebhookEnabled bool `json:"webhook_enabled,omitempty"`
}

// TunnelDesired reports whether the outbound gateway tunnel should be up: any of
// the three remote surfaces (MCP / gRPC / webhook) is enabled AND a gateway URL
// is configured. It is the single source of truth for that predicate — the
// manager's desiredState.on() (internal/server/remote/manager.go) delegates here
// for the dial decision, and the TUI's remote-connection indicator uses it to
// light exactly when the tunnel will actually dial. Without a gateway URL the
// tunnel is a documented no-op, so the indicator stays dark rather than showing
// a misleading "connecting" state forever.
func (s RemoteMcpSettings) TunnelDesired() bool {
	return (s.Enabled || s.FleetEnabled || s.WebhookEnabled) && strings.TrimSpace(s.GatewayURL) != ""
}
