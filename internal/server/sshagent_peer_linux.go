//go:build linux

package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

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

// instanceIdentity is what an instance's socket knows about its container.
type instanceIdentity interface {
	// containerID is the recorded container ID ("" before `devcontainer up`
	// has returned).
	containerID() string
	// workspaceDir is the instance's workspace folder — the
	// devcontainer.local_folder label of its container.
	workspaceDir() string
}

// agentPeerAllowed admits the daemon's own user and root and, for an
// instance's socket, any process of that instance's own container, whatever
// uid its image runs it as (a remoteUser whose uid could not be remapped, a
// rootless-docker subuid). inst is nil for the host socket. Everyone else —
// other users on the host, who can traverse the control directory — is
// refused.
func agentPeerAllowed(conn net.Conn, inst instanceIdentity) bool {
	cred, ok := peerCred(conn)
	if !ok {
		return false
	}
	if cred.Uid == 0 || int(cred.Uid) == os.Getuid() {
		return true
	}
	if inst == nil {
		return false
	}
	return pidInInstanceContainer(cred.Pid, inst)
}

// procRoot is /proc; a var so tests can point it at a fixture.
var procRoot = "/proc"

// runtimeComponent matches the cgroup a container runtime creates for a
// container: /docker/<id> (cgroupfs driver), docker-<id>.scope (systemd),
// libpod-<id>.scope (podman), crio-/cri-containerd- likewise.
var runtimeComponent = regexp.MustCompile(`^(?:docker-|libpod-|crio-|cri-containerd-)?([0-9a-f]{64})(?:\.scope)?$`)

// processCgroup returns pid's cgroup path: the unified (v2) entry, else the
// systemd v1 hierarchy, else the first v1 entry.
func processCgroup(pid int) (string, bool) {
	data, err := os.ReadFile(procRoot + "/" + strconv.Itoa(pid) + "/cgroup")
	if err != nil {
		return "", false
	}
	var systemd, first string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		switch {
		case parts[0] == "0" && parts[1] == "":
			return parts[2], true
		case parts[1] == "name=systemd":
			systemd = parts[2]
		case first == "":
			first = parts[2]
		}
	}
	if systemd != "" {
		return systemd, true
	}
	return first, first != ""
}

// runtimeAnchor cuts a cgroup path after its LAST runtime-created component
// (the innermost container, for nested runtimes) and returns that prefix and
// the container ID it names ("" when the path is in no container).
func runtimeAnchor(path string) (prefix, id string) {
	parts := strings.Split(path, "/")
	for i := len(parts) - 1; i >= 0; i-- {
		if m := runtimeComponent.FindStringSubmatch(parts[i]); m != nil {
			return strings.Join(parts[:i+1], "/"), m[1]
		}
	}
	return "", ""
}

// pidInInstanceContainer reports whether pid runs inside the instance's
// container. The container ID in the peer's cgroup path is NOT enough on its
// own: another user can name a cgroup of their own after it (a delegated
// systemd user scope called docker-<id>.scope). So the peer's path, cut at
// its runtime component, must equal the container's real one — taken from
// the container's own init process — which only the runtime (root, or the
// daemon user for rootless) can create cgroups under. The container must
// then be this instance's: the recorded ID, or — while `devcontainer up` is
// still running and nothing is recorded yet (postCreate, dotfiles) — a
// container labelled with this instance's workspace folder.
func pidInInstanceContainer(pid int32, inst instanceIdentity) bool {
	path, ok := processCgroup(int(pid))
	if !ok {
		return false
	}
	prefix, id := runtimeAnchor(path)
	if id == "" {
		return false
	}
	want := inst.containerID()
	if id != want && inst.workspaceDir() == "" {
		return false
	}
	info, ok := lookupContainer(id)
	if !ok || info.anchor != prefix {
		return false
	}
	return id == want || (info.localFolder != "" && info.localFolder == inst.workspaceDir())
}

// containerInfo is what the peer check needs about one container.
type containerInfo struct {
	anchor      string // the container's cgroup, cut at its runtime component
	localFolder string // its devcontainer.local_folder label
}

// inspectContainer asks the runtime for a container's init pid and workspace
// label. A var so tests need no docker.
var inspectContainer = func(id string) (pid int, localFolder string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "inspect", "-f",
		`{{.State.Pid}}|{{index .Config.Labels "devcontainer.local_folder"}}`, id).Output()
	if err != nil {
		return 0, "", err
	}
	pidText, folder, _ := strings.Cut(strings.TrimSpace(string(out)), "|")
	pid, err = strconv.Atoi(pidText)
	if err != nil || pid <= 0 {
		return 0, "", fmt.Errorf("container %s is not running", id)
	}
	return pid, folder, nil
}

// containerCache remembers containerInfo per ID (a container's cgroup and
// labels never change), and failed lookups for a short while so a peer
// cannot make the daemon run docker for every connection.
var containerCache = struct {
	sync.Mutex
	found  map[string]containerInfo
	failed map[string]time.Time
}{found: map[string]containerInfo{}, failed: map[string]time.Time{}}

const containerLookupRetry = 10 * time.Second

func lookupContainer(id string) (containerInfo, bool) {
	containerCache.Lock()
	if info, ok := containerCache.found[id]; ok {
		containerCache.Unlock()
		return info, true
	}
	if at, ok := containerCache.failed[id]; ok && time.Since(at) < containerLookupRetry {
		containerCache.Unlock()
		return containerInfo{}, false
	}
	containerCache.Unlock()

	info, ok := resolveContainer(id)
	containerCache.Lock()
	defer containerCache.Unlock()
	if !ok {
		containerCache.failed[id] = time.Now()
		return containerInfo{}, false
	}
	delete(containerCache.failed, id)
	containerCache.found[id] = info
	return info, true
}

func resolveContainer(id string) (containerInfo, bool) {
	pid, folder, err := inspectContainer(id)
	if err != nil {
		return containerInfo{}, false
	}
	path, ok := processCgroup(pid)
	if !ok {
		return containerInfo{}, false
	}
	anchor, anchorID := runtimeAnchor(path)
	if anchorID != id {
		return containerInfo{}, false
	}
	return containerInfo{anchor: anchor, localFolder: folder}, true
}

// afterInstanceBind is a test hook run between bind and chmod — the window a
// process in the instance could swap the socket for a symlink in.
var afterInstanceBind func(dir, name string)

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
func listenInstanceAgentSocket(dir, name string, inst instanceIdentity, serve func(net.Conn)) (*agentListener, error) {
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
	if afterInstanceBind != nil {
		afterInstanceBind(dir, name)
	}

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
		ln:   ln,
		path: dir + "/" + name,
		done: make(chan struct{}),
		inst: inst,
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
