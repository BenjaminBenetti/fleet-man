package execas

import (
	"bytes"
	"context"
	"path"
	"regexp"
	"strings"
)

// DefaultCacheDir is where a pod keeps its probe cache: in /opt/fleet/run, the
// pod's runtime emptyDir, which the agent and exec-as share and which goes
// with the pod.
const DefaultCacheDir = "/opt/fleet/run/execas"

// ProbeMode is devcontainer.json's userEnvProbe: which kind of shell the exec
// class starts once to read the user's environment.
type ProbeMode string

const (
	ProbeNone                  ProbeMode = "none"
	ProbeLoginShell            ProbeMode = "loginShell"
	ProbeInteractiveShell      ProbeMode = "interactiveShell"
	ProbeLoginInteractiveShell ProbeMode = "loginInteractiveShell"
)

// Effective is the mode a probe runs in: the CLI's default,
// loginInteractiveShell, when the config names none.
func (m ProbeMode) Effective() ProbeMode {
	if m == "" {
		return ProbeLoginInteractiveShell
	}
	return m
}

// Valid reports whether m is a mode devcontainer.json accepts ("" included).
func (m ProbeMode) Valid() bool {
	switch m {
	case "", ProbeNone, ProbeLoginShell, ProbeInteractiveShell, ProbeLoginInteractiveShell:
		return true
	}
	return false
}

// ProbeTarget is one environment probe: the shell to start, as whom, and with
// which environment and directory. Host.Plan builds it for the exec class.
type ProbeTarget struct {
	Mode     ProbeMode
	Shell    string
	Identity Identity
	// Switch is Plan.Switch: whether the probe runs with Identity's
	// credentials (false in single-uid mode, where it runs as this process).
	Switch bool
	// Env is the environment the shell starts with: the container's, as
	// `docker exec -u` would give it.
	Env []string
	Dir string
}

// EnvProber reads a user's environment the way the devcontainer CLI's
// userEnvProbe does. ProbeEnv returns the variables the probed shell exported,
// or nil when the probe failed or timed out; the exec still runs, without them,
// as it does with the CLI.
type EnvProber interface {
	ProbeEnv(ctx context.Context, target ProbeTarget) map[string]string
}

// probeArgs is the shell argv the CLI probes with: script runs between two
// markers in a login and/or interactive shell (pwsh takes its own flags).
func probeArgs(mode ProbeMode, shell, script string) []string {
	if base := path.Base(shell); base == "pwsh" || base == "pwsh-preview" {
		if mode == ProbeLoginInteractiveShell || mode == ProbeLoginShell {
			return []string{shell, "-Login", "-Command", script}
		}
		return []string{shell, "-Command", script}
	}
	flag := "-c"
	switch mode {
	case ProbeLoginInteractiveShell:
		flag = "-lic"
	case ProbeLoginShell:
		flag = "-lc"
	case ProbeInteractiveShell:
		flag = "-ic"
	}
	return []string{shell, flag, script}
}

// probeScript prints the shell's environment between two copies of mark: sep
// separates the entries (NUL for /proc/self/environ, newline for printenv).
func probeScript(mark, dump string) string {
	return "echo -n " + mark + "; " + dump + "; echo -n " + mark
}

// The two ways the CLI dumps the environment: /proc first, printenv when that
// gave nothing.
const (
	probeDumpProc     = "cat /proc/self/environ"
	probeDumpPrintenv = "printenv"
)

// parseProbeOutput extracts the environment between the two marks in out,
// split on sep, the way the CLI parses it: entries without '=' are dropped, a
// later entry wins, and PWD (the probe's own directory) is removed. ok is false
// when the marks are missing, or nothing is between them.
func parseProbeOutput(out []byte, mark string, sep byte) (map[string]string, bool) {
	re := regexp.MustCompile(regexp.QuoteMeta(mark) + `([\s\S]*)` + regexp.QuoteMeta(mark))
	m := re.FindSubmatch(out)
	if m == nil || len(m[1]) == 0 {
		return nil, false
	}
	env := make(map[string]string)
	for entry := range bytes.SplitSeq(m[1], []byte{sep}) {
		name, value, ok := strings.Cut(string(entry), "=")
		if !ok || name == "" {
			continue
		}
		env[name] = value
	}
	delete(env, "PWD")
	return env, true
}
