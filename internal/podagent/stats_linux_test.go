//go:build linux

package podagent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeCgroups lays out a cgroupfs under a temp dir and the /proc/self/cgroup
// naming this process's group in it.
func fakeCgroups(t *testing.T, self string, files map[string]string) *statsSampler {
	t.Helper()
	root := t.TempDir()
	for name, data := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	proc := filepath.Join(t.TempDir(), "cgroup")
	if err := os.WriteFile(proc, []byte(self), 0o644); err != nil {
		t.Fatal(err)
	}
	return &statsSampler{root: root, self: proc}
}

// The container's usage, from its own group when the agent runs in pod-init's
// fleet-agent child: CPU from the counter's growth over wall time, memory
// less the inactive file cache.
func TestStatsReadsTheContainersCgroupV2(t *testing.T) {
	ctr := "kubepods/burstable/pod1/ctr"
	s := fakeCgroups(t, "0::/"+ctr+"/fleet-agent\n", map[string]string{
		ctr + "/cpu.stat":       "usage_usec 1000000\nuser_usec 1\n",
		ctr + "/memory.current": "209715200\n",
		ctr + "/memory.stat":    "anon 1\ninactive_file 104857600\n",
	})
	s.last = &cpuSample{usage: 500 * time.Millisecond, at: time.Now().Add(-time.Second)}
	got, err := s.stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.MemoryBytes != 100<<20 {
		t.Errorf("memory %d, want usage less the inactive cache (%d)", got.MemoryBytes, 100<<20)
	}
	// 0.5s of CPU over about a second of wall time: about 500 millicores.
	if got.CPUMillicores < 400 || got.CPUMillicores > 510 {
		t.Errorf("cpu %.0f millicores, want about 500", got.CPUMillicores)
	}
}

// With no recent sample the first call samples a short window itself; a
// cgroup v1 node reads the controllers' own files.
func TestStatsSamplesAWindowOnV1(t *testing.T) {
	s := fakeCgroups(t, "12:memory:/pod/ctr\n3:cpu,cpuacct:/pod/ctr\n", map[string]string{
		"cpuacct/pod/ctr/cpuacct.usage":        "123456789\n",
		"memory/pod/ctr/memory.usage_in_bytes": "1048576\n",
		"memory/pod/ctr/memory.stat":           "total_inactive_file 4096\n",
	})
	start := time.Now()
	got, err := s.stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < statsWindow {
		t.Error("a first call returned before its sampling window")
	}
	if got.CPUMillicores != 0 || got.MemoryBytes != 1048576-4096 {
		t.Errorf("an idle v1 container: %+v", got)
	}
	if _, err := fakeCgroups(t, "", nil).stats(context.Background()); err == nil {
		t.Error("no cgroup at all: want an error")
	}
}
