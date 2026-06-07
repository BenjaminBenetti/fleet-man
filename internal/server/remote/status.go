package remote

import "github.com/BenjaminBenetti/fleet-man/fleetgrpc"

// status.go builds the fleetgrpc.RemoteMcpStatus values the manager publishes to
// the hub (and thence to the TUI over Watch). The status is the ONLY channel for
// the computed Public MCP URL — it is never persisted in Config.

func statusDisabled() *fleetgrpc.RemoteMcpStatus {
	return &fleetgrpc.RemoteMcpStatus{State: fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_UNSPECIFIED}
}

func statusConnecting() *fleetgrpc.RemoteMcpStatus {
	return &fleetgrpc.RemoteMcpStatus{State: fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_CONNECTING}
}

func statusConnected(publicURL string) *fleetgrpc.RemoteMcpStatus {
	return &fleetgrpc.RemoteMcpStatus{
		State:     fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_CONNECTED,
		PublicUrl: publicURL,
	}
}

func statusError(err error) *fleetgrpc.RemoteMcpStatus {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	return &fleetgrpc.RemoteMcpStatus{
		State: fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_ERROR,
		Error: msg,
	}
}
