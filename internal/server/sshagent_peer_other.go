//go:build !linux

package server

import (
	"errors"
	"net"
)

// agentPeerAllowed relies on permissions alone off Linux: the only relay
// socket there is the host one, in a 0700 directory.
func agentPeerAllowed(net.Conn, string) bool { return true }

// agentInstanceSocketsSupported: per-instance sockets are Linux-only (the
// relay mode; on macOS instances use Docker Desktop's agent socket).
const agentInstanceSocketsSupported = false

func listenInstanceAgentSocket(string, string, func() string, func(net.Conn)) (*agentListener, error) {
	return nil, errors.New("per-instance agent sockets are only supported on Linux")
}
