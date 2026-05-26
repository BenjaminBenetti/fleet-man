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

	// GhMount enables a shared host directory at
	// ~/.fleet/workspaces/<fleet>/.config/gh that is bind-mounted into
	// every instance of this fleet at <containerHome>/.config/gh, so the
	// GitHub CLI stays logged in across instances and across instance
	// churn. The directory holds gh's hosts.yml (oauth_token + user) and
	// config.yml.
	GhMount bool `json:"ghMount,omitempty"`

	// HomeDir is the absolute path inside the container that should be
	// treated as the home directory when computing mount targets — e.g.
	// "/home/vscode" so a Claude Code mount lands at "/home/vscode/.claude".
	// Empty means "not yet known"; the TUI's edit-fleet dialog tries to
	// auto-detect a value (from devcontainer.json or the image's USER
	// directive) when the user enables a mount, and the resolver falls
	// back to "/home/vscode" if it stays empty at provisioning time.
	HomeDir string `json:"homeDir,omitempty"`

	// PreferFleetLaunch controls which page the built-in browser opens to
	// when a workspace's devcontainer.json configures BOTH a
	// customizations.fleet.browser.initialUrl AND a landingPage.sites
	// list. When only one of the two is configured, it has no effect.
	//
	// It is a pointer so "never set" (nil) is distinct from an explicit
	// false: the first time the browser is opened on a fleet whose
	// workspace has both configured and this is still nil, the TUI prompts
	// the user to choose and stores their answer here. false → initialUrl
	// wins; true → the Fleet Launch landing page wins.
	PreferFleetLaunch *bool `json:"preferFleetLaunch,omitempty"`
}

// PreferFleetLaunchSet reports whether a browser-start preference has ever
// been chosen for this fleet (vs. nil, "never asked").
func (s FleetSettings) PreferFleetLaunchSet() bool {
	return s.PreferFleetLaunch != nil
}

// PreferFleetLaunchEnabled reports whether the Fleet Launch landing page
// should win over initialUrl when both are configured. Defaults to false
// when unset.
func (s FleetSettings) PreferFleetLaunchEnabled() bool {
	return s.PreferFleetLaunch != nil && *s.PreferFleetLaunch
}
