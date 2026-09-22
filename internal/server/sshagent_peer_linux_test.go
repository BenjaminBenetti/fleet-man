//go:build linux

package server

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
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

// fakeRuntime answers inspectContainer for the given containers (id → init
// pid + workspace label) and clears the lookup cache around the test.
func fakeRuntime(t *testing.T, containers map[string]struct {
	pid    int
	folder string
}) *int {
	t.Helper()
	calls := 0
	orig := inspectContainer
	inspectContainer = func(id string) (int, string, error) {
		calls++
		c, ok := containers[id]
		if !ok {
			return 0, "", errors.New("no such container")
		}
		return c.pid, c.folder, nil
	}
	reset := func() {
		containerCache.Lock()
		containerCache.found = map[string]containerInfo{}
		containerCache.failed = map[string]time.Time{}
		containerCache.Unlock()
	}
	reset()
	t.Cleanup(func() {
		inspectContainer = orig
		reset()
	})
	return &calls
}

func TestPidInInstanceContainer(t *testing.T) {
	const id = "3f1c0a9e5b7d2c4e6f8a0b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e"
	const other = "0000000000000000000000000000000000000000000000000000000000000001"
	write := procFixture(t)
	calls := fakeRuntime(t, map[string]struct {
		pid    int
		folder string
	}{
		id:    {pid: 10, folder: "/ws/f/i/f"},
		other: {pid: 20, folder: "/ws/f/j/f"},
	})
	scope := "/system.slice/docker-" + id + ".scope"
	write(10, "0::"+scope+"/init\n") // the container's init (moved by DinD)
	write(20, "0::/system.slice/docker-"+other+".scope\n")
	write(100, "0::"+scope+"\n")                                                                   // an exec'd process in the container
	write(101, "0::"+scope+"/sub/cgroup\n")                                                        // a process in a child cgroup
	write(102, "0::/user.slice/user-1001.slice/user@1001.service/app.slice/docker-"+id+".scope\n") // ANOTHER user's cgroup named after it
	write(103, "0::/system.slice/docker-"+other+".scope\n")                                        // another instance's container
	write(104, "0::/user.slice/user-1000.slice/session-3.scope\n")                                 // a host process

	recorded := fakeInstance{id: id, workspace: "/ws/f/i/f"}
	upcoming := fakeInstance{id: "", workspace: "/ws/f/i/f"} // devcontainer up has not returned
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
		{"gone", 999, recorded, false},
		{"container not recorded yet, recognized by its workspace label", 100, upcoming, true},
		{"not recorded yet, another instance's container", 103, upcoming, false},
		{"not recorded and no workspace known", 100, fakeInstance{}, false},
	}
	for _, c := range cases {
		if got := pidInInstanceContainer(c.pid, c.inst); got != c.want {
			t.Errorf("%s: allowed = %v, want %v", c.name, got, c.want)
		}
	}
	if *calls > 2 {
		t.Errorf("docker inspect ran %d times; container info must be cached", *calls)
	}
}
