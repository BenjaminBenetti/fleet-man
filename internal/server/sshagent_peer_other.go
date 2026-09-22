//go:build !linux

package server

import "net"

// agentPeerAllowed relies on permissions alone off Linux: the only relay
// socket there is the host one, in a 0700 directory.
func agentPeerAllowed(net.Conn) bool { return true }
