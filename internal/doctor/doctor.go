// Package doctor provides the "doctor" diagnostic feature, which invokes a
// coding agent to diagnose and fix the user's fleet-man setup.
package doctor

import (
	"fmt"
	"os/exec"

	"github.com/BenjaminBenetti/fleet-man/internal/agent"
)

// ===========================================
// Constants
// ===========================================

const doctorURL = "https://raw.githubusercontent.com/BenjaminBenetti/fleet-man/main/skills/DOCTOR.md"

// DoctorPrompt is the instruction sent to the coding agent.
var DoctorPrompt = fmt.Sprintf("Read %s and follow the instructions", doctorURL)

// ===========================================
// Public API
// ===========================================

// FindAgent returns the name and binary of the first available coding agent
// on PATH. Returns an error if none are found.
func FindAgent() (name string, binary string, err error) {
	return agent.FindAgent()
}

// Command returns an unstarted *exec.Cmd for the doctor session.
// The caller is responsible for attaching I/O (e.g. via tea.ExecProcess).
func Command() (*exec.Cmd, error) {
	return agent.CommandWithPrompt(DoctorPrompt)
}

// Run finds the first available coding agent and launches it interactively
// with the doctor prompt. Blocks until the agent exits.
func Run() error {
	cmd, err := Command()
	if err != nil {
		return err
	}
	return agent.RunInteractive(cmd)
}
