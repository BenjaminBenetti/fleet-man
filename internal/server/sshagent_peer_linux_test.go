//go:build linux

package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPidInContainer(t *testing.T) {
	root := t.TempDir()
	orig := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = orig })

	const id = "3f1c0a9e5b7d2c4e6f8a0b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e"
	write := func(pid, cgroup string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, pid), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, pid, "cgroup"), []byte(cgroup), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("100", "0::/system.slice/docker-"+id+".scope\n")                       // docker, systemd driver
	write("101", "12:pids:/docker/"+id+"\n0::/docker/"+id+"\n")                  // cgroupfs driver
	write("102", "0::/user.slice/user-1000.slice/user@1000.service/app.slice\n") // a host process
	write("103", "0::/system.slice/docker-0000000000000000000000000000000000000000000000000000000000000000.scope\n")

	cases := []struct {
		pid  int32
		id   string
		want bool
	}{
		{100, id, true},
		{101, id, true},
		{102, id, false},
		{103, id, false}, // another container
		{100, "", false}, // container not known yet
		{100, "3f1c", false},
		{999, id, false}, // gone
	}
	for _, c := range cases {
		if got := pidInContainer(c.pid, c.id); got != c.want {
			t.Errorf("pidInContainer(%d, %q) = %v, want %v", c.pid, c.id, got, c.want)
		}
	}
}
