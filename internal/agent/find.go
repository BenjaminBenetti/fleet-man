// Package agent provides detection and invocation of coding-agent CLI tools
// (Claude Code, Codex, Gemini, Copilot) installed on the local machine.
package agent

// FindAgent returns the name and binary of the first available coding agent
// on PATH. Returns an error if none are found.
func FindAgent() (name string, binary string, err error) {
	entry, err := findAgentEntry()
	if err != nil {
		return "", "", err
	}
	return entry.Name, entry.Binary, nil
}
