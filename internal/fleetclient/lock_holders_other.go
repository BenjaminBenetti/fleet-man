//go:build !linux

package fleetclient

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// lockHolders lists the processes, other than this one, that have the file at
// path open — for the daemon's lifetime lock, the daemon itself (and any
// redundant spawn still waiting on it). There is no /proc to read here, so it
// asks lsof.
func lockHolders(path string) ([]int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "lsof", "-t", "--", path).Output()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return nil, err
	}
	// lsof exits non-zero when nothing has the file open; the output decides.
	var pids []int
	for field := range strings.FieldsSeq(string(out)) {
		if pid, err := strconv.Atoi(field); err == nil && pid != os.Getpid() {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}
