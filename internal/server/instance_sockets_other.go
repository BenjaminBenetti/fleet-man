//go:build !linux

package server

import (
	"errors"
	"net"
)

// peerAllowed relies on permissions alone off Linux: the only socket served
// there is the SSH-agent relay's host one, in a 0700 directory.
func peerAllowed(net.Conn, instanceIdentity, chan struct{}) bool { return true }

// instanceIdentity is what an instance's socket knows about its container
// (unused off Linux).
type instanceIdentity interface {
	containerID() string
	workspaceDir() string
}

// instanceSocketsSupported: sockets inside instances are Linux-only (a host
// socket cannot cross into Docker Desktop's VM; there instances use Docker
// Desktop's own agent socket, and have no in-instance fleet MCP).
const instanceSocketsSupported = false

func listenInstanceSocket(string, string, string, instanceIdentity, func(net.Conn)) (*socketListener, error) {
	return nil, errors.New("sockets inside instances are only supported on Linux")
}
