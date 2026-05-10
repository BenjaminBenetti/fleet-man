package fleet

// FleetSettings holds per-fleet configuration that influences how the
// fleet's instances are provisioned. These settings are user-controlled
// (today via the TUI edit-fleet dialog) and persisted alongside the
// fleet record in state.json.
//
// Each field declares the user's *intent*; whether the intent is honored
// at provisioning time depends on the target backend. For mount-related
// settings, backends that report SupportsCustomMounts()==false silently
// skip them — the fleet-level toggle stays on for any future instance
// created on a supporting backend.
type FleetSettings struct {
	// ClaudeCodeMount enables a shared host directory at
	// ~/.fleet/workspaces/<fleet>/.claude that is bind-mounted into every
	// instance of this fleet. Used to share Claude Code login/state
	// across instances and across sessions.
	ClaudeCodeMount bool `json:"claudeCodeMount,omitempty"`

	// CodexMount enables a shared host directory at
	// ~/.fleet/workspaces/<fleet>/.codex that is bind-mounted into every
	// instance of this fleet. Used to share Codex login/state across
	// instances and across sessions.
	CodexMount bool `json:"codexMount,omitempty"`

	// HomeDir is the absolute path inside the container that should be
	// treated as the home directory when computing mount targets — e.g.
	// "/home/vscode" so a Claude Code mount lands at "/home/vscode/.claude".
	// Empty means "not yet known"; the TUI's edit-fleet dialog tries to
	// auto-detect a value (from devcontainer.json or the image's USER
	// directive) when the user enables a mount, and the resolver falls
	// back to "/home/vscode" if it stays empty at provisioning time.
	HomeDir string `json:"homeDir,omitempty"`
}
