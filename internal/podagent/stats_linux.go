//go:build linux

package podagent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// stats_linux.go is the agent's "stats" RPC: the dev container's usage from
// its cgroup, as docker stats computes a container's. CPU is the growth of
// the container's usage counter over wall time since the last call (taken
// over a short window when there is no recent one), memory its usage less
// the inactive file cache.

// Cgroup locations.
const (
	cgroupRoot     = "/sys/fs/cgroup"
	procSelfCgroup = "/proc/self/cgroup"
	// statsWindow is how long a first sample waits for its second.
	statsWindow = 250 * time.Millisecond
	// statsStale is how old the last sample may be to compute CPU from.
	statsStale = 30 * time.Second
)

// cpuSample is the container's CPU usage counter at a moment.
type cpuSample struct {
	usage time.Duration
	at    time.Time
}

// statsSampler keeps the last sample between calls.
type statsSampler struct {
	root, self string

	mu   sync.Mutex
	last *cpuSample
}

func newStatsSampler() *statsSampler {
	return &statsSampler{root: cgroupRoot, self: procSelfCgroup}
}

// stats answers the "stats" RPC.
func (s *statsSampler) stats(ctx context.Context) (podwire.StatsInfo, error) {
	cg, err := s.containerCgroup()
	if err != nil {
		return podwire.StatsInfo{}, err
	}
	now := time.Now()
	usage, err := cg.cpuUsage()
	if err != nil {
		return podwire.StatsInfo{}, err
	}
	s.mu.Lock()
	prev := s.last
	s.mu.Unlock()
	if prev == nil || now.Sub(prev.at) > statsStale || now.Sub(prev.at) <= 0 {
		first := &cpuSample{usage: usage, at: now}
		select {
		case <-time.After(statsWindow):
		case <-ctx.Done():
			return podwire.StatsInfo{}, ctx.Err()
		}
		prev = first
		now = time.Now()
		if usage, err = cg.cpuUsage(); err != nil {
			return podwire.StatsInfo{}, err
		}
	}
	s.mu.Lock()
	s.last = &cpuSample{usage: usage, at: now}
	s.mu.Unlock()
	out := podwire.StatsInfo{}
	if wall := now.Sub(prev.at); wall > 0 && usage >= prev.usage {
		out.CPUMillicores = float64(usage-prev.usage) / float64(wall) * 1000
	}
	if mem, err := cg.memoryUsage(); err == nil {
		out.MemoryBytes = mem
	}
	return out, nil
}

// cgroupDir is one container cgroup: a cgroup v2 directory, or v1's
// controller directories.
type cgroupDir struct {
	v2              string
	cpuacct, memory string
}

// containerCgroup finds the dev container's cgroup from this process's: the
// container's own, or its parent when pod-init moved the agent into its
// fleet-agent child.
func (s *statsSampler) containerCgroup() (cgroupDir, error) {
	data, err := os.ReadFile(s.self)
	if err != nil {
		return cgroupDir{}, err
	}
	var cg cgroupDir
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		parts := strings.SplitN(sc.Text(), ":", 3)
		if len(parts) != 3 {
			continue
		}
		p := containerOf(parts[2])
		switch {
		case parts[0] == "0" && parts[1] == "":
			cg.v2 = path.Join(s.root, p)
		case hasController(parts[1], "cpuacct"):
			cg.cpuacct = path.Join(s.root, "cpuacct", p)
		case hasController(parts[1], "memory"):
			cg.memory = path.Join(s.root, "memory", p)
		}
	}
	if cg.cpuacct != "" || cg.memory != "" {
		// A v1 hierarchy: its controllers, not the v2 stub.
		cg.v2 = ""
	}
	if cg.v2 == "" && cg.cpuacct == "" {
		return cgroupDir{}, errors.New("no cgroup for the container in " + s.self)
	}
	return cg, nil
}

// containerOf is the container's cgroup path for this process's: the parent
// of pod-init's child groups (fleet-agent, fleet-user and its procs).
func containerOf(p string) string {
	if path.Base(p) == "procs" && path.Base(path.Dir(p)) == "fleet-user" {
		p = path.Dir(p)
	}
	switch path.Base(p) {
	case "fleet-agent", "fleet-user":
		return path.Dir(p)
	}
	return p
}

func hasController(list, name string) bool {
	for _, c := range strings.Split(list, ",") {
		if c == name {
			return true
		}
	}
	return false
}

// cpuUsage is the container's total CPU time.
func (cg cgroupDir) cpuUsage() (time.Duration, error) {
	if cg.v2 != "" {
		v, err := statField(path.Join(cg.v2, "cpu.stat"), "usage_usec")
		return time.Duration(v) * time.Microsecond, err
	}
	n, err := readInt(path.Join(cg.cpuacct, "cpuacct.usage"))
	return time.Duration(n), err
}

// memoryUsage is the container's memory use as docker stats shows it.
func (cg cgroupDir) memoryUsage() (int64, error) {
	usageFile, statFile, inactive := path.Join(cg.v2, "memory.current"), path.Join(cg.v2, "memory.stat"), "inactive_file"
	if cg.v2 == "" {
		usageFile, statFile, inactive = path.Join(cg.memory, "memory.usage_in_bytes"), path.Join(cg.memory, "memory.stat"), "total_inactive_file"
	}
	usage, err := readInt(usageFile)
	if err != nil {
		return 0, err
	}
	if cache, err := statField(statFile, inactive); err == nil && cache < usage {
		usage -= cache
	}
	return usage, nil
}

func readInt(file string) (int64, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
}

// statField is a "name value" line's value in a cgroup stat file.
func statField(file, name string) (int64, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(line, " "); ok && k == name {
			return strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		}
	}
	return 0, fmt.Errorf("%s has no %s", file, name)
}
