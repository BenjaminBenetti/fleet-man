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

// peerIdentity is who is on the other end of a relay connection: the
// kernel-reported credentials (SO_PEERCRED: uid and pid as seen from this
// daemon's namespaces — for a process in a container, its host uid and host
// pid) and, where the kernel offers it (6.5+), a pidfd pinning that exact
// process, so a pid reused after the connector exited cannot be mistaken for
// it.
type peerIdentity struct {
	uid   uint32
	pid   int32
	pidfd int // -1 when unavailable
}

func peerCred(conn net.Conn) (peerIdentity, bool) {
	id := peerIdentity{pidfd: -1}
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return id, false
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return id, false
	}
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		var cred *unix.Ucred
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if credErr != nil {
			return
		}
		id.uid, id.pid = cred.Uid, cred.Pid
		pidfd, err := unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_PEERPIDFD)
		switch {
		case err == nil:
			id.pidfd = pidfd
		case errors.Is(err, unix.ENOPROTOOPT) || errors.Is(err, unix.EOPNOTSUPP):
			// A kernel without SO_PEERPIDFD (< 6.5): fall back to the uid check.
		default:
			// The connector is already gone (EINVAL / ESRCH: exited and
			// reaped) — exactly when its pid may name someone else.
			credErr = err
		}
	}); err != nil || credErr != nil {
		id.close()
		return id, false
	}
	return id, true
}

func (id peerIdentity) close() {
	if id.pidfd >= 0 {
		_ = unix.Close(id.pidfd)
	}
}

// stillTheConnector reports whether pid still names the process that
// connected, so what was read from /proc/<pid> was about it. With a pidfd:
// that process has not exited (EPERM means alive, just not ours to signal).
// Without one (older kernels): the process now at pid runs as the uid that
// connected — a reused pid in a container almost always runs as another uid
// than the user who planted the stale connection.
func (id peerIdentity) stillTheConnector() bool {
	if id.pidfd >= 0 {
		err := unix.PidfdSendSignal(id.pidfd, 0, nil, 0)
		return err == nil || errors.Is(err, unix.EPERM)
	}
	data, err := os.ReadFile(procRoot + "/" + strconv.Itoa(int(id.pid)) + "/status")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if fields := strings.Fields(line); len(fields) >= 3 && fields[0] == "Uid:" {
			return fields[2] == strconv.FormatUint(uint64(id.uid), 10) // effective uid
		}
	}
	return false
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
// instance's socket, any process of that instance's own container (or of a
// container nested in it), whatever uid its image runs it as — a remoteUser
// whose uid could not be remapped, a rootless-docker subuid. inst is nil for
// the host socket. Everyone else — other users on the host, who can traverse
// the control directory — is refused. The container check may run docker
// (cached): call this off the accept loop. slow, when non-nil, bounds how
// many of those checks run at once for the socket; past it a cross-uid peer
// is refused rather than queued, so a flood of them cannot pile up docker
// processes (the daemon's own user never waits on it).
func agentPeerAllowed(conn net.Conn, inst instanceIdentity, slow chan struct{}) bool {
	peer, ok := peerCred(conn)
	if !ok {
		return false
	}
	defer peer.close()
	if peer.uid == 0 || int(peer.uid) == os.Getuid() {
		return true
	}
	if inst == nil {
		return false
	}
	if slow != nil {
		select {
		case slow <- struct{}{}:
			defer func() { <-slow }()
		default:
			return false
		}
	}
	return peerInInstanceContainer(peer, inst)
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
	anchors := runtimeAnchors(path)
	if len(anchors) == 0 {
		return "", ""
	}
	return anchors[0].prefix, anchors[0].id
}

type cgroupAnchor struct{ prefix, id string }

// runtimeAnchors lists every runtime-created component of a cgroup path,
// innermost first, each with the path cut after it.
func runtimeAnchors(path string) []cgroupAnchor {
	parts := strings.Split(path, "/")
	var out []cgroupAnchor
	for i := len(parts) - 1; i >= 0; i-- {
		if m := runtimeComponent.FindStringSubmatch(parts[i]); m != nil {
			out = append(out, cgroupAnchor{prefix: strings.Join(parts[:i+1], "/"), id: m[1]})
		}
	}
	return out
}

// peerInInstanceContainer reports whether the peer runs inside the
// instance's container, or in a container nested inside it (a devcontainer
// running docker). A container ID merely appearing in the peer's cgroup path
// is NOT enough: another user can name a cgroup of their own after it (a
// delegated systemd user scope called docker-<id>.scope). So only this
// instance's own containers are considered — the recorded ID, or while
// `devcontainer up` is still running and nothing is recorded yet (postCreate,
// dotfiles), the containers labelled with this instance's workspace folder —
// and the peer's path, cut at that container's component, must equal the
// container's real one, taken from its own init process: only the runtime
// (root, or the daemon user for rootless) can create cgroups under it. No ID
// a peer chooses is ever looked up.
func peerInInstanceContainer(peer peerIdentity, inst instanceIdentity) bool {
	path, ok := processCgroup(int(peer.pid))
	if !ok {
		return false
	}
	anchors := runtimeAnchors(path)
	if len(anchors) == 0 {
		return false
	}
	matches := func(ours map[string]bool) bool {
		for _, a := range anchors {
			if !ours[a.id] {
				continue
			}
			if info, ok := lookupContainer(a.id); ok && info.anchor == a.prefix {
				// The cgroup read must have been about the process that
				// connected, not one that took its pid since.
				return peer.stillTheConnector()
			}
		}
		return false
	}
	// The recorded container first: the common case costs no docker call.
	if id := inst.containerID(); id != "" && matches(map[string]bool{id: true}) {
		return true
	}
	labelled := map[string]bool{}
	for _, id := range containersLabelled(inst.workspaceDir()) {
		labelled[id] = true
	}
	return len(labelled) > 0 && matches(labelled)
}

// containerInfo is what the peer check needs about one container.
type containerInfo struct {
	anchor string // the container's cgroup, cut at its runtime component
}

// inspectContainer asks the runtime for a container's init pid. A var so
// tests need no docker.
var inspectContainer = func(id string) (pid int, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.State.Pid}}", id).Output()
	if err != nil {
		return 0, err
	}
	pid, err = strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("container %s is not running", id)
	}
	return pid, nil
}

// listLabelled lists the IDs of the containers labelled with a workspace
// folder. A var so tests need no docker.
var listLabelled = func(workspaceDir string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "ps", "-q", "--no-trunc",
		"--filter", "label=devcontainer.local_folder="+workspaceDir).Output()
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

// labelledTTL bounds how long a workspace's labelled containers are reused:
// long enough that a burst of connections costs one docker call, short
// enough that a container just created by `devcontainer up` is seen.
const labelledTTL = 2 * time.Second

var labelledCache = struct {
	sync.Mutex
	entries  map[string]labelledEntry
	inflight map[string]chan struct{}
}{entries: map[string]labelledEntry{}, inflight: map[string]chan struct{}{}}

type labelledEntry struct {
	ids []string
	at  time.Time
}

// containersLabelled returns the containers labelled with workspaceDir
// ("" or a docker failure → none). Every answer, failures included, is reused
// for labelledTTL, and concurrent callers for the same workspace share one
// docker call.
func containersLabelled(workspaceDir string) []string {
	if workspaceDir == "" {
		return nil
	}
	labelledCache.Lock()
	for {
		if e, ok := labelledCache.entries[workspaceDir]; ok && time.Since(e.at) < labelledTTL {
			labelledCache.Unlock()
			return e.ids
		}
		wait, busy := labelledCache.inflight[workspaceDir]
		if !busy {
			break
		}
		labelledCache.Unlock()
		<-wait
		labelledCache.Lock()
	}
	done := make(chan struct{})
	labelledCache.inflight[workspaceDir] = done
	labelledCache.Unlock()

	ids, err := listLabelled(workspaceDir)
	if err != nil {
		ids = nil
	}
	labelledCache.Lock()
	labelledCache.entries[workspaceDir] = labelledEntry{ids: ids, at: time.Now()}
	delete(labelledCache.inflight, workspaceDir)
	labelledCache.Unlock()
	close(done)
	return ids
}

// containerCache remembers containerInfo per ID (a container's cgroup never
// changes) and failed lookups for a short while (a stopped container named by
// a peer must not cost a docker call per connection). Only this daemon's own
// instances' containers are ever looked up, so it stays small. Concurrent
// lookups of one ID share a docker call.
var containerCache = struct {
	sync.Mutex
	found    map[string]containerInfo
	failed   map[string]time.Time
	inflight map[string]chan struct{}
}{found: map[string]containerInfo{}, failed: map[string]time.Time{}, inflight: map[string]chan struct{}{}}

// containerLookupRetry is how long a failed lookup is not repeated.
const containerLookupRetry = 10 * time.Second

func lookupContainer(id string) (containerInfo, bool) {
	containerCache.Lock()
	for {
		if info, ok := containerCache.found[id]; ok {
			containerCache.Unlock()
			return info, true
		}
		if at, ok := containerCache.failed[id]; ok && time.Since(at) < containerLookupRetry {
			containerCache.Unlock()
			return containerInfo{}, false
		}
		wait, busy := containerCache.inflight[id]
		if !busy {
			break
		}
		containerCache.Unlock()
		<-wait
		containerCache.Lock()
	}
	done := make(chan struct{})
	containerCache.inflight[id] = done
	containerCache.Unlock()

	info, ok := resolveContainer(id)
	containerCache.Lock()
	if ok {
		containerCache.found[id] = info
		delete(containerCache.failed, id)
	} else {
		containerCache.failed[id] = time.Now()
	}
	delete(containerCache.inflight, id)
	containerCache.Unlock()
	close(done)
	return info, ok
}

func resolveContainer(id string) (containerInfo, bool) {
	pid, err := inspectContainer(id)
	if err != nil {
		return containerInfo{}, false
	}
	path, ok := processCgroup(pid)
	if !ok {
		return containerInfo{}, false
	}
	var anchor string
	for _, a := range runtimeAnchors(path) {
		if a.id == id {
			anchor = a.prefix
			break
		}
	}
	if anchor == "" {
		return containerInfo{}, false
	}
	return containerInfo{anchor: anchor}, true
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
		ln:         ln,
		path:       dir + "/" + name,
		done:       make(chan struct{}),
		inst:       inst,
		slowChecks: make(chan struct{}, maxSlowPeerChecks),
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
