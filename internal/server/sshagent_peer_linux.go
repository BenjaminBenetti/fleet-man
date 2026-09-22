//go:build linux

package server

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// peerCred reads the connecting process's kernel-reported credentials
// (SO_PEERCRED: its uid and pid as seen from this daemon's namespaces — for a
// process in a container, its host uid and host pid).
func peerCred(conn net.Conn) (*unix.Ucred, bool) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return nil, false
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return nil, false
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil || credErr != nil {
		return nil, false
	}
	return cred, true
}

// agentPeerAllowed admits the daemon's own user and root and, for an
// instance's socket, any process inside that instance's container (whatever
// uid its image runs as — a remoteUser whose uid could not be remapped, a
// rootless-docker subuid). containerID is "" for the host socket, or while
// the instance's container is not known yet. Everyone else — other users on
// the host, who can traverse the control directory — is refused.
func agentPeerAllowed(conn net.Conn, containerID string) bool {
	cred, ok := peerCred(conn)
	if !ok {
		return false
	}
	if cred.Uid == 0 || int(cred.Uid) == os.Getuid() {
		return true
	}
	return containerID != "" && pidInContainer(cred.Pid, containerID)
}

// procRoot is /proc; a var so tests can point it at a fixture.
var procRoot = "/proc"

// pidInContainer reports whether pid runs inside the container: container
// runtimes (docker, podman, rootless or not, cgroup v1 or v2) put the full
// container ID in the cgroup path of every process in it.
func pidInContainer(pid int32, containerID string) bool {
	if pid <= 0 || len(containerID) < 12 {
		return false
	}
	data, err := os.ReadFile(procRoot + "/" + strconv.Itoa(int(pid)) + "/cgroup")
	if err != nil {
		return false
	}
	return strings.Contains(string(data), containerID)
}

// agentInstanceSocketsSupported reports whether per-instance sockets can be
// served here.
const agentInstanceSocketsSupported = true

// listenInstanceAgentSocket serves an instance's relay socket in its control
// directory, which the instance can write to. It never resolves a path the
// instance controls: the directory is held open (O_PATH, no symlink follow)
// and the socket is bound through /proc/self/fd/<dir>/<name> — which also
// keeps the bind address short however long the fleet and instance names are
// (a unix socket path is capped at 108 bytes). The mode is then set through a
// no-follow handle on the socket itself, so a symlink swapped in by a process
// in the instance can never redirect a chmod onto a host file.
func listenInstanceAgentSocket(dir, name string, containerID func() string, serve func(net.Conn)) (*agentListener, error) {
	dirFD, err := unix.Open(dir, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dir, err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = unix.Close(dirFD)
		}
	}()

	var st unix.Stat_t
	switch err := unix.Fstatat(dirFD, name, &st, unix.AT_SYMLINK_NOFOLLOW); {
	case err == nil:
		if st.Mode&unix.S_IFMT != unix.S_IFSOCK {
			return nil, fmt.Errorf("%s/%s exists and is not a socket", dir, name)
		}
		if err := unix.Unlinkat(dirFD, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return nil, fmt.Errorf("remove stale agent socket: %w", err)
		}
	case !errors.Is(err, unix.ENOENT):
		return nil, fmt.Errorf("stat %s/%s: %w", dir, name, err)
	}

	addr := "/proc/self/fd/" + strconv.Itoa(dirFD) + "/" + name
	ln, err := net.Listen("unix", addr)
	if err != nil {
		return nil, err
	}
	// Closing must not unlink by path: the fd number in addr will be gone, and
	// the entry may be someone else's by then (see Close).
	ln.(*net.UnixListener).SetUnlinkOnClose(false)

	sockFD, err := unix.Openat(dirFD, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("open agent socket: %w", err)
	}
	defer unix.Close(sockFD)
	if err := unix.Fstat(sockFD, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFSOCK {
		_ = ln.Close()
		return nil, fmt.Errorf("agent socket %s/%s was replaced while binding", dir, name)
	}
	// 0666: a container user with another uid must be able to connect; who
	// gets served is decided per connection by agentPeerAllowed.
	if err := os.Chmod("/proc/self/fd/"+strconv.Itoa(sockFD), 0o666); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("chmod agent socket: %w", err)
	}

	l := &agentListener{
		ln:          ln,
		path:        dir + "/" + name,
		done:        make(chan struct{}),
		containerID: containerID,
	}
	ino, dev := st.Ino, st.Dev
	l.unlink = func() {
		// Remove the entry only if it is still the socket this listener bound.
		var cur unix.Stat_t
		if unix.Fstatat(dirFD, name, &cur, unix.AT_SYMLINK_NOFOLLOW) == nil && cur.Ino == ino && cur.Dev == dev {
			_ = unix.Unlinkat(dirFD, name, 0)
		}
		_ = unix.Close(dirFD)
	}
	ok = true
	go l.acceptLoop(serve)
	return l, nil
}
