//go:build linux

package podagent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
)

// takeInitOOMScore gives this process o.InitOOMScore's value, if set.
func takeInitOOMScore(o Options, log *Logger) {
	if o.InitOOMScore == "" {
		return
	}
	if err := adoptOOMScore(o.InitOOMScore, "/proc/self/oom_score_adj"); err != nil {
		log.Warn("could not take the container's OOM score", "err", err.Error())
	}
}

// adoptOOMScore writes the oom_score_adj read from from (PID 1's in a pod) to
// to (this process's), so the agent and everything it starts are no more
// shielded from the OOM killer than the container's own processes: CRI-O with
// crun starts exec'd processes at -999, which would leave PID 1 the kernel's
// victim and take the whole container down. Raising the score needs no
// privilege; lowering it needs CAP_SYS_RESOURCE, and without that the agent
// keeps its own (more killable) score. Only a failed raise is an error.
func adoptOOMScore(from, to string) error {
	want, err := readOOMScore(from)
	if err != nil {
		return err
	}
	have, err := readOOMScore(to)
	if err != nil {
		return err
	}
	if have == want {
		return nil
	}
	err = os.WriteFile(to, []byte(strconv.Itoa(want)), 0)
	if err != nil && want < have && errors.Is(err, fs.ErrPermission) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("set oom_score_adj %d: %w", want, err)
	}
	return nil
}

func readOOMScore(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || v < -1000 || v > 1000 {
		return 0, fmt.Errorf("%s: bad oom_score_adj %q", path, strings.TrimSpace(string(data)))
	}
	return v, nil
}
