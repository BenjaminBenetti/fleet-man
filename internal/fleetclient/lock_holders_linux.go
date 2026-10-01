//go:build linux

package fleetclient

import (
	"os"
	"path/filepath"
	"strconv"
)

// lockHolders lists the processes, other than this one, that have the file at
// path open — for the daemon's lifetime lock, the daemon itself (and any
// redundant spawn still waiting on it). It reads /proc, so it finds only
// processes in this PID namespace that this user may inspect: a pid it
// returns is always safe to signal.
func lockHolders(path string) ([]int, error) {
	want, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, proc := range procs {
		pid, err := strconv.Atoi(proc.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		fdDir := filepath.Join("/proc", proc.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue // another user's process, or one that just exited
		}
		for _, fd := range fds {
			if info, err := os.Stat(filepath.Join(fdDir, fd.Name())); err == nil && os.SameFile(want, info) {
				pids = append(pids, pid)
				break
			}
		}
	}
	return pids, nil
}
