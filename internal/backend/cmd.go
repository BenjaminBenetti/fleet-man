package backend

import (
	"os/exec"
	"time"
)

// Cmd wraps an *exec.Cmd built by a Backend's ExecCommand so the command's
// completion can be observed even though the caller — not the backend — runs
// it. exec.Cmd offers no completion hook (a context only interrupts the
// process; there is no "on exit" callback), so we embed *exec.Cmd and shadow
// the three run-to-completion methods (Run, Output, CombinedOutput), invoking
// an onDone callback the backend registered. The embedded *exec.Cmd is
// promoted, so callers still set Stdin/Stdout/Stderr, read Args, etc. exactly
// as they would on a plain *exec.Cmd.
//
// Callers that need the raw *exec.Cmd — e.g. to hand to tea.ExecProcess, which
// requires that concrete type — reach it through the embedded field: cmd.Cmd.
type Cmd struct {
	*exec.Cmd
	// onDone, if non-nil, is called with the run duration and result after
	// Run/Output/CombinedOutput finish. ExecCommand sets it to log a single
	// timed "container exec" event; ExecCommandQuiet leaves it nil so polling
	// loops stay silent.
	onDone func(time.Duration, error)
}

// NewCmd wraps raw with an optional completion callback. A nil onDone makes
// the run methods behave exactly like the underlying *exec.Cmd.
func NewCmd(raw *exec.Cmd, onDone func(time.Duration, error)) *Cmd {
	return &Cmd{Cmd: raw, onDone: onDone}
}

// Run shadows exec.Cmd.Run, timing the call and firing onDone on completion.
func (c *Cmd) Run() error {
	start := time.Now()
	err := c.Cmd.Run()
	c.fireDone(start, err)
	return err
}

// Output shadows exec.Cmd.Output. exec.Cmd.Output internally calls the
// embedded Run via static dispatch (not this wrapper's Run), so onDone fires
// exactly once per call.
func (c *Cmd) Output() ([]byte, error) {
	start := time.Now()
	out, err := c.Cmd.Output()
	c.fireDone(start, err)
	return out, err
}

// CombinedOutput shadows exec.Cmd.CombinedOutput, with the same single-fire
// guarantee as Output.
func (c *Cmd) CombinedOutput() ([]byte, error) {
	start := time.Now()
	out, err := c.Cmd.CombinedOutput()
	c.fireDone(start, err)
	return out, err
}

func (c *Cmd) fireDone(start time.Time, err error) {
	if c.onDone != nil {
		c.onDone(time.Since(start), err)
	}
}
