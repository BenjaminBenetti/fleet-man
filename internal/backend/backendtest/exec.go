package backendtest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// commandTimeout bounds one helper command: generous, since a cold backend's
// first exec can be slow, but finite so a hung exec fails the case.
const commandTimeout = 5 * time.Minute

// Result is how a command run inside an instance ended.
type Result struct {
	Stdout, Stderr []byte
	// Code is the exit code, -1 when the command did not exit on its own (it
	// could not start, a signal killed it, or it timed out).
	Code int
	// Err is nil exactly when the command exited 0.
	Err error
}

// String describes the result for a failure message.
func (r Result) String() string {
	return fmt.Sprintf("exit %d (err %v)\nstdout: %s\nstderr: %s", r.Code, r.Err, clip(r.Stdout), clip(r.Stderr))
}

// runCmd runs cmd with stdin (nil: none), capturing stdout and stderr, and
// kills it after timeout.
func runCmd(cmd *exec.Cmd, stdin io.Reader, timeout time.Duration) Result {
	var stdout, stderr bytes.Buffer
	cmd.Stdin = stdin
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// A grandchild holding the pipes open must not wedge Wait past a kill.
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return Result{Code: -1, Err: err}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-done
		err = fmt.Errorf("timed out after %s", timeout)
	}
	return Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Code: exitCode(err), Err: err}
}

// exitCode is the exit code err reports: 0 for nil, the code for an exit
// status, -1 otherwise (a signal, a start or wait failure, a timeout).
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// Exec runs argv in the instance through ExecCommand, feeding it stdin (nil:
// none).
func (c *C) Exec(i *Instance, argv []string, stdin io.Reader) Result {
	return runCmd(i.B.ExecCommand(i.WorkspaceDir(), argv).Cmd, stdin, commandTimeout)
}

// ExecQuiet is Exec through ExecCommandQuiet.
func (c *C) ExecQuiet(i *Instance, argv []string, stdin io.Reader) Result {
	return runCmd(i.B.ExecCommandQuiet(i.WorkspaceDir(), argv).Cmd, stdin, commandTimeout)
}

// ShArgv is the argv running script under sh with args as $1, $2, ...
func ShArgv(script string, args ...string) []string {
	return append([]string{"sh", "-c", script, "sh"}, args...)
}

// Sh runs script under sh in the instance through ExecCommand, with args as
// $1, $2, ..., fails the case unless it exits 0, and returns its stdout without
// the trailing newline.
func (c *C) Sh(i *Instance, script string, args ...string) string {
	c.Helper()
	r := c.Exec(i, ShArgv(script, args...), nil)
	if r.Err != nil {
		c.Fatalf("exec %q: %s", script, r)
	}
	return strings.TrimRight(string(r.Stdout), "\n")
}

// Script runs script through RunScript (as the session user, straight against
// the container), fails the case on an error, and returns its output without
// the trailing newline.
func (c *C) Script(i *Instance, script string) string {
	c.Helper()
	out, err := i.B.RunScript(i.ID(), script)
	if err != nil {
		c.Fatalf("RunScript %q: %v\n%s", script, err, clip([]byte(out)))
	}
	return strings.TrimRight(out, "\n")
}

// Home is the instance user's $HOME.
func (c *C) Home(i *Instance) string {
	c.Helper()
	return c.Sh(i, `printf '%s' "$HOME"`)
}

// Scratch makes a fresh folder for the case under the instance user's $HOME
// and returns its path; it is removed when the case ends.
func (c *C) Scratch(i *Instance) string {
	c.Helper()
	dir := c.Home(i) + "/.fleet-conformance/" + c.name + "-" + randHex(4)
	c.Sh(i, `mkdir -p "$1"`, dir)
	c.Cleanup(func() {
		// Best effort: the instance may already be gone.
		_ = c.Exec(i, []string{"rm", "-rf", dir}, nil)
	})
	return dir
}

// Eventually calls cond until it reports true or timeout passes, and fails the
// case with the last detail cond gave otherwise.
func (c *C) Eventually(what string, timeout time.Duration, cond func() (bool, string)) {
	c.Helper()
	var detail string
	timeout = c.within(timeout)
	if poll(c.Ctx, timeout, func() bool {
		var ok bool
		ok, detail = cond()
		return ok
	}) {
		return
	}
	c.Fatalf("timed out after %s waiting for %s: %s", timeout, what, detail)
}

// within scales a wait bound by the suite's patience.
func (c *C) within(d time.Duration) time.Duration { return c.suite.within(d) }

// NeedTool skips the rest of a check on a HostProcesses target that lacks
// tool, and reports whether the check can go on. On every other target the
// fixture provides tool, so a missing one fails the case.
func (c *C) NeedTool(i *Instance, tool string) bool {
	c.Helper()
	r := c.Exec(i, ShArgv(`command -v "$1"`, tool), nil)
	if r.Err == nil {
		return true
	}
	if c.Target().HostProcesses {
		c.Logf("skipping the %s check: %s is not on this host", tool, tool)
		return false
	}
	c.Fatalf("the fixture's %s is missing from the instance: %s", tool, r)
	return false
}

// clip shortens output for a failure message.
func clip(b []byte) string {
	const max = 2000
	s := string(b)
	if len(s) > max {
		return s[:max] + fmt.Sprintf("... (%d more bytes)", len(s)-max)
	}
	return s
}
