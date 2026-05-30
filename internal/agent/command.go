package agent

import "os/exec"

// CommandWithPrompt returns an unstarted *exec.Cmd that invokes the first
// available coding agent with the given prompt.
// The caller is responsible for attaching I/O (e.g. via tea.ExecProcess).
func CommandWithPrompt(prompt string) (*exec.Cmd, error) {
	agent, err := findAgentEntry()
	if err != nil {
		return nil, err
	}
	return exec.Command(agent.Binary, agent.Args(prompt)...), nil
}
