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

// DefaultProbeDeps are the files besides the startup files whose state a
// probed environment depends on in a pod: the in-pod sockets the agent binds
// in the control directory that a shell's startup tests, fleet.rc (it hands
// agents the fleet MCP only while mcp.sock is there) or a user's rc files (the
// agent socket). A socket bound or unbound changes the fingerprint, so a
// cached environment never outlives the socket state it was probed with.
// fleet.sock is not one: it is bound while a fleet client is attached, so it
// comes and goes with every TUI attach and detach, and a dep on it would have
// every pod probe its user's environment again each time; fleet.rc does not
// test it.
var DefaultProbeDeps = []string{
	"/fleet-mounts/control/ssh-agent.sock",
	"/fleet-mounts/control/mcp.sock",
}

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
	// Deps are files besides the shell's startup files whose state its
	// answer depends on (Host.ProbeDeps); they count in the cache's
	// fingerprint.
	Deps []string
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
