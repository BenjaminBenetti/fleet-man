//go:build linux

package podinit

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// forwardedSignals are the signals PID 1 passes on: every catchable one but
// SIGCHLD (pod-init's own business) and the synchronous faults no one sends
// from outside, as tini.
func forwardedSignals() []os.Signal {
	sigs := []os.Signal{unix.SIGHUP, unix.SIGINT, unix.SIGQUIT, unix.SIGUSR1, unix.SIGUSR2,
		unix.SIGALRM, unix.SIGTERM, unix.SIGCONT, unix.SIGTSTP, unix.SIGTTIN, unix.SIGTTOU,
		unix.SIGWINCH, unix.SIGPWR, unix.SIGXCPU, unix.SIGVTALRM, unix.SIGPROF, unix.SIGIO}
	for n := sigRTMin; n <= sigRTMax; n++ {
		sigs = append(sigs, syscall.Signal(n))
	}
	return sigs
}

// caughtSignals reads the signals process pid has a handler for (SigCgt).
func caughtSignals(pid int) (uint64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, err
	}
	return parseSigCgt(string(data))
}

// parseSigCgt is the SigCgt mask of a /proc/<pid>/status.
func parseSigCgt(status string) (uint64, error) {
	for line := range strings.SplitSeq(status, "\n") {
		if rest, ok := strings.CutPrefix(line, "SigCgt:"); ok {
			return strconv.ParseUint(strings.TrimSpace(rest), 16, 64)
		}
	}
	return 0, fmt.Errorf("no SigCgt in the process status")
}

// catches reports whether mask has a handler for sig.
func catches(mask uint64, sig syscall.Signal) bool {
	return sig >= 1 && sig <= 64 && mask&(1<<(uint(sig)-1)) != 0
}

// rlimitResources maps the --ulimit names to Linux's resources.
var rlimitResources = map[string]int{
	"core": unix.RLIMIT_CORE, "cpu": unix.RLIMIT_CPU, "data": unix.RLIMIT_DATA,
	"fsize": unix.RLIMIT_FSIZE, "locks": unix.RLIMIT_LOCKS, "memlock": unix.RLIMIT_MEMLOCK,
	"msgqueue": unix.RLIMIT_MSGQUEUE, "nice": unix.RLIMIT_NICE, "nofile": unix.RLIMIT_NOFILE,
	"nproc": unix.RLIMIT_NPROC, "rss": unix.RLIMIT_RSS, "rtprio": unix.RLIMIT_RTPRIO,
	"rttime": unix.RLIMIT_RTTIME, "sigpending": unix.RLIMIT_SIGPENDING, "stack": unix.RLIMIT_STACK,
	"as": unix.RLIMIT_AS,
}

func rlimValue(v int64) uint64 {
	if v < 0 {
		return unix.RLIM_INFINITY
	}
	return uint64(v)
}

// applyRlimits sets the --ulimit values on this process, which the start
// script inherits. Raising a hard limit needs CAP_SYS_RESOURCE; without it the
// hard limit is kept, the soft one clamped under it, and a warning returned.
func applyRlimits(limits []Rlimit) []string {
	var warnings []string
	for _, l := range limits {
		res, ok := rlimitResources[l.Name]
		if !ok {
			continue
		}
		want := unix.Rlimit{Cur: rlimValue(l.Soft), Max: rlimValue(l.Hard)}
		err := unix.Setrlimit(res, &want)
		if err == nil {
			continue
		}
		var cur unix.Rlimit
		if gerr := unix.Getrlimit(res, &cur); gerr != nil {
			warnings = append(warnings, fmt.Sprintf("ulimit %s: %v", l.Name, err))
			continue
		}
		clamped := unix.Rlimit{Cur: min(want.Cur, cur.Max), Max: cur.Max}
		if cerr := unix.Setrlimit(res, &clamped); cerr != nil {
			warnings = append(warnings, fmt.Sprintf("ulimit %s: %v", l.Name, cerr))
			continue
		}
		warnings = append(warnings, fmt.Sprintf("ulimit %s: raising the hard limit to %s needs CAP_SYS_RESOURCE; kept %s (soft %s)",
			l.Name, limitString(want.Max), limitString(cur.Max), limitString(clamped.Cur)))
	}
	return warnings
}

func limitString(v uint64) string {
	if v == unix.RLIM_INFINITY {
		return "unlimited"
	}
	return strconv.FormatUint(v, 10)
}
