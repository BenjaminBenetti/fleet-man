// Package devcontainersetup hands the user off to a local coding-agent
// CLI for guided creation of a devcontainer.json. It mirrors
// internal/doctor: the only meaningful difference is the prompt the
// agent receives — instead of diagnosing fleet-man itself, the agent
// clones the user's repository, walks them through writing a basic
// devcontainer, and consults the live Microsoft docs so the result
// reflects current schema rather than stale training data.
package devcontainersetup

import (
	"fmt"
	"os/exec"

	"github.com/BenjaminBenetti/fleet-man/internal/agent"
)

// ===========================================
// Constants
// ===========================================

// skillURL points at the skill markdown describing the step-by-step
// devcontainer-setup workflow. Hosting the instructions on GitHub —
// rather than baking them into the binary — lets us iterate on the
// workflow without shipping a new fleet-man release every time.
const skillURL = "https://raw.githubusercontent.com/BenjaminBenetti/fleet-man/main/skills/DEVCONTAINER_SETUP.md"

// ===========================================
// Public API
// ===========================================

// Prompt returns the instruction sent to the coding agent.
// remoteURL is the git remote of the fleet's repository — the agent
// will clone it locally so the user can collaboratively author the
// new .devcontainer/devcontainer.json before pushing.
func Prompt(remoteURL string) string {
	return fmt.Sprintf(
		"Read %s and follow the instructions. The repository to set up is %s. "+
			"Make sure to consult the latest devcontainer documentation on the web "+
			"so the configuration you produce reflects the current schema and features.",
		skillURL, remoteURL,
	)
}

// FindAgent returns the name and binary of the first available coding
// agent on PATH. Returns an error if none are found. Re-exposed at this
// layer so the TUI can render an "agent: claude" hint without importing
// the lower-level agent package directly.
func FindAgent() (name string, binary string, err error) {
	return agent.FindAgent()
}

// Command returns an unstarted *exec.Cmd that launches the first
// available coding agent with the setup prompt for remoteURL.
// The caller is responsible for attaching I/O (e.g. via tea.ExecProcess).
func Command(remoteURL string) (*exec.Cmd, error) {
	return agent.CommandWithPrompt(Prompt(remoteURL))
}

// Run finds the first available coding agent and launches it
// interactively with the setup prompt. Blocks until the agent exits.
// Intended for non-TUI callers; the TUI uses Command + tea.ExecProcess
// so bubbletea can yield the terminal cleanly.
func Run(remoteURL string) error {
	cmd, err := Command(remoteURL)
	if err != nil {
		return err
	}
	return agent.RunInteractive(cmd)
}
