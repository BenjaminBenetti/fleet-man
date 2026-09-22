//go:build linux

package server

import (
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// agentPeerAllowed admits only the daemon's own user and root, by the
// connecting process's kernel-reported credentials (SO_PEERCRED; for a process
// in a container this is its uid on the host). The socket is 0600 as well;
// this also closes the window between bind and chmod.
func agentPeerAllowed(conn net.Conn) bool {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return false
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return false
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil || credErr != nil {
		return false
	}
	return cred.Uid == 0 || int(cred.Uid) == os.Getuid()
}
