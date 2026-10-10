package kubeshim

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/redact"
	"github.com/charmbracelet/x/term"
)

// Exit codes the shim itself returns. Everything else is the exit code of
// what it ran (the real docker, or the command in the pod).
const (
	// ExitCaptured ends the captured docker run: the devcontainer CLI fails
	// there, which is the point, and CaptureUp reads the capture instead.
	ExitCaptured = 97
	// exitShim is the shim refusing or failing a call. Docker uses 125 for
	// errors of docker itself, as opposed to the command's.
	exitShim = 125
)

// globalFlags are docker's own options, which come before the verb.
var globalFlags = newFlagSet([]flagSpec{
	{name: "config", value: true},
	{name: "context", short: "c", value: true},
	{name: "debug", short: "D"},
	{name: "host", short: "H", value: true},
	{name: "log-level", short: "l", value: true},
	{name: "tls"},
	{name: "tlscacert", value: true},
	{name: "tlscert", value: true},
	{name: "tlskey", value: true},
	{name: "tlsverify"},
	{name: "version", short: "v"},
	{name: "help"},
})

// Invoked reports whether a process started as argv0 is the shim: the fleet
// executable reached through a shim directory's docker or docker-compose
// link. cmd/fleet checks it before anything else.
func Invoked(argv0 string) bool {
	base := filepath.Base(argv0)
	return base == dockerLink || base == composeLink
}

// Main runs the shim for this process and returns its exit code.
func Main() int {
	cwd, _ := os.Getwd()
	return run(invocation{
		args:       os.Args,
		stdin:      os.Stdin,
		stdout:     os.Stdout,
		stderr:     os.Stderr,
		getenv:     os.LookupEnv,
		cwd:        cwd,
		exec:       syscall.Exec,
		environ:    os.Environ,
		isTerminal: isTerminal,
		getppid:    os.Getppid,
	})
}

// invocation is what one run of the shim gets from its process, so tests can
// drive it in-process.
type invocation struct {
	args           []string
	stdin          io.Reader
	stdout, stderr io.Writer
	getenv         func(string) (string, bool)
	environ        func() []string
	cwd            string
	// exec replaces the process with another program (syscall.Exec).
	exec func(path string, argv []string, env []string) error
	// isTerminal reports whether stdin is a terminal (pod mode's exec -t).
	isTerminal func(r io.Reader) bool
	// getppid is the parent's pid (os.Getppid), which a blocked `docker
	// events` watches.
	getppid func() int
	// poll is how often a blocked `docker events` checks whether it may end;
	// zero means eventsPoll.
	poll time.Duration
}

// shim is one call being served.
type shim struct {
	inv     invocation
	binary  string // "docker" or "docker-compose"
	dir     string
	cfg     *Config
	args    []string // argv after the binary
	globals []Flag
	verb    string
	rest    []string // argv after the verb
}

func run(inv invocation) int {
	if len(inv.args) == 0 {
		fmt.Fprintln(inv.stderr, "kubeshim: no argv")
		return exitShim
	}
	s := &shim{inv: inv, binary: filepath.Base(inv.args[0]), args: inv.args[1:]}
	dir, err := shimDir(inv.args[0])
	if err != nil {
		fmt.Fprintf(inv.stderr, "fleet: invoked as %s outside a kubeshim directory: %v\n", s.binary, err)
		return exitShim
	}
	s.dir = dir
	if s.cfg, err = loadConfig(dir); err != nil {
		fmt.Fprintf(inv.stderr, "fleet: invoked as %s, but %s has no usable %s: %v\n", s.binary, dir, ConfigFile, err)
		return exitShim
	}
	if s.binary == composeLink {
		return s.compose()
	}
	globals, verb, rest, err := splitGlobals(s.args)
	if err != nil {
		return s.refuse(err.Error())
	}
	s.globals, s.verb, s.rest = globals, verb, rest
	switch s.cfg.Mode {
	case ModeCapture:
		return s.captureMode()
	default:
		return s.podMode()
	}
}

// shimDir is the directory of the link the shim was started through. The
// devcontainer CLI runs it by the absolute path it was given; a bare name was
// found on PATH.
func shimDir(argv0 string) (string, error) {
	path := argv0
	if !strings.Contains(argv0, "/") {
		found, err := exec.LookPath(argv0)
		if err != nil {
			return "", err
		}
		path = found
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.Dir(abs), nil
}

// splitGlobals splits a docker argv into docker's own options, the verb and
// what follows it. A bare `docker -v` has no verb.
func splitGlobals(args []string) ([]Flag, string, []string, error) {
	globals, rest, err := globalFlags.parse(args)
	if err != nil {
		return nil, "", nil, err
	}
	if len(rest) == 0 {
		return globals, "", nil, nil
	}
	return globals, rest[0], rest[1:], nil
}

// globalsArgs is the argv that came before the verb.
func (s *shim) globalsArgs() []string {
	n := len(s.args) - len(s.rest)
	if s.verb != "" {
		n--
	}
	return append([]string(nil), s.args[:n]...)
}

// record adds this call to the directory's journal.
func (s *shim) record(outcome Outcome, detail string) {
	appendCall(filepath.Join(s.dir, callsFile), Call{
		Mode:    s.cfg.Mode,
		Binary:  s.binary,
		Args:    s.args,
		Outcome: outcome,
		Detail:  detail,
	})
}

// refuse fails a call the shim does not serve, loudly: the devcontainer CLI
// shows the message in its log, and the journal keeps it for the job's error.
func (s *shim) refuse(why string) int {
	s.record(OutcomeRefused, why)
	fmt.Fprintf(s.inv.stderr, "kubeshim (%s mode): refusing `%s %s`: %s\n", s.cfg.Mode, s.binary, strings.Join(redact.EnvArgs(s.args), " "), why)
	return exitShim
}

// compose refuses every compose call, whichever binary it came through.
func (s *shim) compose() int {
	s.record(OutcomeCompose, "")
	fmt.Fprintf(s.inv.stderr, "%s: compose devcontainers (dockerComposeFile) are not supported on Kubernetes\n", UnsupportedCompose)
	return 1
}

// isTerminal reports whether r is a terminal.
func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	return ok && term.IsTerminal(f.Fd())
}
