package podspec

import (
	"fmt"
	"strconv"
	"strings"
)

// The options about the container's process: what pod-init (pid1.json)
// carries out as the container's PID 1.

func (t *translation) optInit(vals []string) Outcome {
	t.spec.PID1.Init = lastBool(vals)
	return ""
}

func (t *translation) optInteractive(vals []string) Outcome {
	t.spec.Stdin = lastBool(vals)
	return ""
}

func (t *translation) optTTY(vals []string) Outcome {
	t.spec.TTY = lastBool(vals)
	return ""
}

func (t *translation) optStopSignal(vals []string) Outcome {
	sig, err := canonicalSignal(last(vals))
	if err != nil {
		t.refuse(refuseOption("stop-signal", err.Error()))
		return ""
	}
	t.spec.PID1.StopSignal = sig
	return ""
}

func (t *translation) optStopTimeout(vals []string) Outcome {
	v := last(vals)
	n, err := strconv.ParseInt(v, 10, 64)
	switch {
	case err != nil:
		t.refuse(refuseOption("stop-timeout", fmt.Sprintf("invalid stop timeout %q", v)))
	case n < 0:
		// docker waits forever; a pod's grace period has to end.
		t.warn("--stop-timeout %d: a pod's grace period cannot be unlimited; the cluster's default applies", n)
		return Delta
	default:
		t.spec.TerminationGracePeriodSeconds = &n
	}
	return ""
}

// ulimitNames are the resources docker's --ulimit takes.
var ulimitNames = map[string]bool{
	"as": true, "core": true, "cpu": true, "data": true, "fsize": true, "locks": true,
	"memlock": true, "msgqueue": true, "nice": true, "nofile": true, "nproc": true,
	"rss": true, "rtprio": true, "rttime": true, "sigpending": true, "stack": true,
}

func (t *translation) optUlimit(vals []string) Outcome {
	byName := map[string]int{}
	for _, v := range vals {
		l, err := parseUlimit(v)
		if err != nil {
			t.refuse(refuseOption("ulimit", err.Error()))
			continue
		}
		// A repeated resource: the last one wins, as in docker.
		if i, ok := byName[l.Name]; ok {
			t.spec.PID1.Rlimits[i] = l
			continue
		}
		byName[l.Name] = len(t.spec.PID1.Rlimits)
		t.spec.PID1.Rlimits = append(t.spec.PID1.Rlimits, l)
	}
	return ""
}

// parseUlimit reads docker's name=soft[:hard] (unlimited or -1 for no
// limit); without a hard limit it equals the soft one.
func parseUlimit(v string) (Rlimit, error) {
	name, limits, ok := strings.Cut(v, "=")
	if !ok || name == "" {
		return Rlimit{}, fmt.Errorf("invalid ulimit argument: %s", v)
	}
	if !ulimitNames[name] {
		return Rlimit{}, fmt.Errorf("invalid ulimit type: %s", name)
	}
	soft, hard, hasHard := strings.Cut(limits, ":")
	s, err := ulimitValue(soft)
	if err != nil {
		return Rlimit{}, fmt.Errorf("invalid ulimit argument: %s", v)
	}
	h := s
	if hasHard {
		if h, err = ulimitValue(hard); err != nil {
			return Rlimit{}, fmt.Errorf("invalid ulimit argument: %s", v)
		}
	}
	if h != -1 && (s == -1 || s > h) {
		return Rlimit{}, fmt.Errorf("ulimit soft limit must be less than or equal to hard limit: %s", v)
	}
	return Rlimit{Name: name, Soft: s, Hard: h}, nil
}

func ulimitValue(s string) (int64, error) {
	if s == "unlimited" {
		return -1, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < -1 {
		return 0, fmt.Errorf("invalid limit %q", s)
	}
	return n, nil
}

func (t *translation) optUmask(vals []string) Outcome {
	v := last(vals)
	n, err := strconv.ParseUint(v, 8, 32)
	if err != nil || n > 0o777 {
		t.refuse(refuseOption("umask", fmt.Sprintf("invalid umask %q: an octal mode such as 0022", v)))
		return ""
	}
	t.spec.PID1.Umask = fmt.Sprintf("%04o", n)
	return ""
}

func (t *translation) optOOMScoreAdj(vals []string) Outcome {
	v := last(vals)
	n, err := strconv.Atoi(v)
	if err != nil || n < -1000 || n > 1000 {
		t.refuse(refuseOption("oom-score-adj", fmt.Sprintf("invalid OOM score %q: between -1000 and 1000", v)))
		return ""
	}
	t.spec.PID1.OOMScoreAdj = &n
	return ""
}

func (t *translation) optRestart(vals []string) Outcome {
	policy, _, _ := strings.Cut(last(vals), ":")
	switch policy {
	case "always", "unless-stopped", "on-failure":
		return ""
	case "no", "":
		t.warn("--restart %s: fleet restarts a crashed pod (and parks a crash loop as stopped); a pod cannot stay stopped after it exits", last(vals))
		return Delta
	}
	t.refuse(refuseOption("restart", fmt.Sprintf("invalid restart policy %q", last(vals))))
	return ""
}

// finishProcess writes what pod-init runs: the command, its user and
// working directory (as docker merges them with the image's), and the
// image's stop signal when the run names none.
func (t *translation) finishProcess() {
	c := t.container
	entrypoint := c.Entrypoint
	// --entrypoint "" resets the image's entrypoint to none.
	if len(entrypoint) == 1 && entrypoint[0] == "" {
		entrypoint = nil
	}
	t.spec.PID1.Format = PID1Format
	t.spec.PID1.Argv = append(append([]string{}, entrypoint...), c.Cmd...)
	if len(t.spec.PID1.Argv) == 0 {
		t.refuse(&Refusal{Code: CodeNoCommand, Reason: "the run has no command and the image no entrypoint or CMD"})
	}
	t.spec.PID1.User = c.User
	t.spec.PID1.WorkingDir = c.WorkingDir
	// A pod without root runs as a uid the image may not name; pod-init
	// gives it remoteUser's home and shell.
	t.spec.PID1.RemoteUser = t.merged.RemoteUser
	if t.spec.PID1.StopSignal == "" && t.in.Image.Config.StopSignal != "" {
		sig, err := canonicalSignal(t.in.Image.Config.StopSignal)
		if err != nil {
			t.warn("the image's STOPSIGNAL %s is not a signal; the pod stops with SIGTERM", t.in.Image.Config.StopSignal)
		} else {
			t.spec.PID1.StopSignal = sig
		}
	}
	t.finishLimits()
}

// finishLimits warns about what pod-init may not be able to do: raising a
// hard rlimit or lowering the OOM score needs CAP_SYS_RESOURCE, which an
// unprivileged pod has only when the run adds it.
func (t *translation) finishLimits() {
	if t.hasCap("SYS_RESOURCE") {
		return
	}
	var hard []string
	for _, l := range t.spec.PID1.Rlimits {
		hard = append(hard, l.Name)
	}
	if len(hard) > 0 {
		t.warn("--ulimit %s: pod-init sets these limits, but cannot raise a hard limit above the pod's own without CAP_SYS_RESOURCE", strings.Join(hard, ", "))
	}
	if adj := t.spec.PID1.OOMScoreAdj; adj != nil && *adj < 0 {
		t.warn("--oom-score-adj %d: lowering the OOM score below the pod's needs CAP_SYS_RESOURCE; pod-init keeps the pod's if it cannot", *adj)
	}
}
