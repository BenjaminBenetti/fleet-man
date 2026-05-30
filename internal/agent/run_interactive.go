package agent

import (
	"os"
	"os/exec"
)

// RunInteractive wires the given command's standard streams to the current
// process's streams and runs it to completion, blocking until it exits.
// It is the canonical way to hand the terminal off to a coding-agent CLI for
// an interactive session (non-TUI callers); TUI callers should instead use
// the unstarted *exec.Cmd directly with tea.ExecProcess so bubbletea can
// yield the terminal cleanly.
func RunInteractive(cmd *exec.Cmd) error {
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
