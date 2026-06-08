package state

// RemoteMcpSettings holds the user's intent to expose this daemon's local MCP
// server to the internet through a remote fleet gateway, so remote agents can
// drive the fleet. The local MCP server itself (internal/server/mcp.go) is
// unchanged and stays loopback-only; the gateway tunnel (a later PR) dials OUT
// to GatewayURL and reverse-proxies inbound requests back to it.
//
// Only two fields are persisted here. The gateway-assigned "Public MCP URL" is
// deliberately NOT stored: it is runtime state the server computes after it
// connects and pushes to the TUI over the Watch stream (fleetgrpc.RemoteMcpStatus).
// Keeping it out of Config is what stops SetConfig — which replaces the whole
// Config — from clobbering it.
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
}
