//go:build linux

package server

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeInstance struct{ id, workspace string }

func (f fakeInstance) containerID() string  { return f.id }
func (f fakeInstance) workspaceDir() string { return f.workspace }

func TestRuntimeAnchor(t *testing.T) {
	const id = "3f1c0a9e5b7d2c4e6f8a0b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e"
	const inner = "aaaa0a9e5b7d2c4e6f8a0b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e"
	cases := []struct {
		path, prefix, id string
	}{
		{"/system.slice/docker-" + id + ".scope", "/system.slice/docker-" + id + ".scope", id},
		{"/system.slice/docker-" + id + ".scope/init", "/system.slice/docker-" + id + ".scope", id}, // DinD moves init
		{"/docker/" + id, "/docker/" + id, id},
		{"/user.slice/user-1000.slice/user@1000.service/user.slice/libpod-" + id + ".scope/container", "/user.slice/user-1000.slice/user@1000.service/user.slice/libpod-" + id + ".scope", id},
		{"/docker/" + id + "/docker/" + inner, "/docker/" + id + "/docker/" + inner, inner}, // nested: the innermost
		{"/user.slice/user-1000.slice/session-2.scope", "", ""},
		{"/system.slice/docker-" + id[:12] + ".scope", "", ""},
	}
	for _, c := range cases {
		prefix, got := runtimeAnchor(c.path)
		if prefix != c.prefix || got != c.id {
			t.Errorf("runtimeAnchor(%q) = %q, %q; want %q, %q", c.path, prefix, got, c.prefix, c.id)
		}
	}
}

// procFixture points procRoot at a fake /proc and returns a writer of
// /proc/<pid>/cgroup files.
func procFixture(t *testing.T) func(pid int, cgroup string) {
	t.Helper()
	root := t.TempDir()
	orig := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = orig })
	return func(pid int, cgroup string) {
		t.Helper()
		dir := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte(cgroup), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// fakeRuntime answers the docker seams for the given containers (id → init
// pid) and labels (workspace → ids), counts calls, and clears the caches.
type fakeDocker struct {
	inspected []string
	listed    int
}

func fakeRuntime(t *testing.T, containers map[string]int, labels map[string][]string) *fakeDocker {
	t.Helper()
	fd := &fakeDocker{}
	origInspect, origList := inspectContainer, listLabelled
	inspectContainer = func(id string) (int, error) {
		fd.inspected = append(fd.inspected, id)
		pid, ok := containers[id]
		if !ok {
			return 0, errors.New("no such container")
		}
		return pid, nil
	}
	listLabelled = func(ws string) ([]string, error) {
		fd.listed++
		return labels[ws], nil
	}
	reset := func() {
		containerCache.Lock()
		containerCache.found = map[string]containerInfo{}
		containerCache.failed = map[string]time.Time{}
		containerCache.Unlock()
		labelledCache.Lock()
		labelledCache.entries = map[string]labelledEntry{}
		labelledCache.Unlock()
	}
	reset()
	t.Cleanup(func() {
		inspectContainer, listLabelled = origInspect, origList
		reset()
	})
	return fd
}

// writeStatus writes /proc/<pid>/status with an effective uid.
func writeStatus(t *testing.T, pid int, uid int) {
	t.Helper()
	dir := filepath.Join(procRoot, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	status := "Name:\tx\nUid:\t" + strconv.Itoa(uid) + "\t" + strconv.Itoa(uid) + "\t" + strconv.Itoa(uid) + "\t" + strconv.Itoa(uid) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPeerInInstanceContainer(t *testing.T) {
	const id = "3f1c0a9e5b7d2c4e6f8a0b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e"
	const other = "0000000000000000000000000000000000000000000000000000000000000001"
	const nested = "2222222222222222222222222222222222222222222222222222222222222222"
	write := procFixture(t)
	docker := fakeRuntime(t, map[string]int{id: 10, other: 20}, map[string][]string{"/ws/f/i/f": {id}, "/ws/f/j/f": {other}})
	scope := "/system.slice/docker-" + id + ".scope"
	write(10, "0::"+scope+"/init\n") // the container's init (moved by DinD)
	write(20, "0::/system.slice/docker-"+other+".scope\n")
	write(100, "0::"+scope+"\n")                                                                   // an exec'd process in the container
	write(101, "0::"+scope+"/sub/cgroup\n")                                                        // a process in a child cgroup
	write(102, "0::/user.slice/user-1001.slice/user@1001.service/app.slice/docker-"+id+".scope\n") // ANOTHER user's cgroup named after it
	write(103, "0::/system.slice/docker-"+other+".scope\n")                                        // another instance's container
	write(104, "0::/user.slice/user-1000.slice/session-3.scope\n")                                 // a host process
	write(105, "0::"+scope+"/docker/"+nested+"\n")                                                 // a container nested in the instance (DinD)
	write(106, "0::/system.slice/docker-"+strings.Repeat("9", 64)+".scope\n")                      // a container a peer picked
	for _, pid := range []int{100, 101, 102, 103, 104, 105, 106} {
		writeStatus(t, pid, 4001)
	}

	recorded := fakeInstance{id: id, workspace: "/ws/f/i/f"}
	upcoming := fakeInstance{id: "", workspace: "/ws/f/i/f"} // devcontainer up has not returned
	peer := func(pid int32) peerIdentity { return peerIdentity{uid: 4001, pid: pid, pidfd: -1} }
	cases := []struct {
		name string
		pid  int32
		inst fakeInstance
		want bool
	}{
		{"process in the container", 100, recorded, true},
		{"process in a child cgroup", 101, recorded, true},
		{"another user's look-alike cgroup", 102, recorded, false},
		{"another instance's container", 103, recorded, false},
		{"host process", 104, recorded, false},
		{"container nested in the instance", 105, recorded, true},
		{"gone", 999, recorded, false},
		{"not recorded yet, recognized by its workspace label", 100, upcoming, true},
		{"not recorded yet, another instance's container", 103, upcoming, false},
		{"not recorded and no workspace known", 100, fakeInstance{}, false},
	}
	for _, c := range cases {
		if got := peerInInstanceContainer(peer(c.pid), c.inst); got != c.want {
			t.Errorf("%s: allowed = %v, want %v", c.name, got, c.want)
		}
	}

	// A peer can make the daemon look up only this instance's own containers,
	// never an ID of its choosing.
	docker.inspected = nil
	peerInInstanceContainer(peer(106), recorded)
	for _, looked := range docker.inspected {
		if looked != id {
			t.Fatalf("docker inspect ran for %s, an ID the peer chose", looked)
		}
	}
}

func TestPeerCheckRejectsAReusedPid(t *testing.T) {
	const id = "3f1c0a9e5b7d2c4e6f8a0b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e"
	write := procFixture(t)
	fakeRuntime(t, map[string]int{id: 10}, nil)
	scope := "/system.slice/docker-" + id + ".scope"
	write(10, "0::"+scope+"\n")
	// Another user (uid 1001) connected, then exited; pid 100 now belongs to
	// a process in the instance's container running as uid 4001.
	write(100, "0::"+scope+"\n")
	writeStatus(t, 100, 4001)
	if peerInInstanceContainer(peerIdentity{uid: 1001, pid: 100, pidfd: -1}, fakeInstance{id: id}) {
		t.Fatal("a connection judged by a reused pid's cgroup must be refused")
	}
	if !peerInInstanceContainer(peerIdentity{uid: 4001, pid: 100, pidfd: -1}, fakeInstance{id: id}) {
		t.Fatal("the process that connected is still the one at the pid: allowed")
	}
}

func TestPeerCheckSharesDockerCallsAndCachesFailures(t *testing.T) {
	const id = "3f1c0a9e5b7d2c4e6f8a0b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e"
	write := procFixture(t)
	docker := fakeRuntime(t, map[string]int{}, map[string][]string{})
	// A stopped recorded container (inspect fails) that a peer names in its
	// cgroup, hit by a burst of connections.
	write(100, "0::/system.slice/docker-"+id+".scope\n")
	writeStatus(t, 100, 4001)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			peerInInstanceContainer(peerIdentity{uid: 4001, pid: 100, pidfd: -1}, fakeInstance{id: id, workspace: "/ws"})
		}()
	}
	wg.Wait()
	if n := len(docker.inspected); n > 1 {
		t.Fatalf("docker inspect ran %d times for one failing container; want it shared and the failure cached", n)
	}
	if docker.listed > 1 {
		t.Fatalf("docker ps ran %d times for one workspace; want it shared and cached", docker.listed)
	}
}
