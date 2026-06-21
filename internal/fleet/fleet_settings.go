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

	// AuggieMount enables a shared host directory at
	// ~/.fleet/workspaces/<fleet>/.augment that is bind-mounted into every
	// instance of this fleet at <containerHome>/.augment. Used to share
	// Auggie (the Augment Code CLI) login/state across instances and across
	// sessions: the directory holds session.json (auth token) and
	// settings.json. Like ClaudeCodeMount and CodexMount it also enables the
	// Auggie auto-install startup script.
	AuggieMount bool `json:"auggieMount,omitempty"`

	// BuildkitServer enables a shared moby/buildkit container for this fleet.
	// When set, instance provisioning ensures one buildkit container per fleet
	// whose unix socket lives at ~/.fleet/workspaces/<fleet>/.buildkit/ and is
	// bind-mounted into every instance; each instance's docker buildx is then
	// pointed at it (a "remote" builder) so all instances share one build
	// cache. Plain bool (default off), matching the *Mount flags: false ==
	// "never set" == disabled.
	//
	// This declares intent only. At provisioning time it is honored solely on
	// backends that report SupportsCustomMounts()==true (devcontainer); cloud
	// backends silently skip it. Even on a supporting backend, setup is
	// best-effort: if docker or buildx is absent inside an instance the
	// configuration is skipped without error (the instance stays usable).
	BuildkitServer bool `json:"buildkitServer,omitempty"`

	// CustomMounts is the list of user-defined shared directory mounts for
	// the fleet. Each entry is an ABSOLUTE in-container path (e.g.
	// "/opt/data"); the host side is derived as
	// ~/.fleet/workspaces/<fleet>/.mnt/<path> and bind-mounted into every
	// instance, so the directory is shared across all instances exactly like
	// the *Mount toggles. Entries are validated/normalized (see
	// NormalizeCustomMount) before persisting. When a custom mount's
	// container path collides with a managed mount (Claude/Codex/Gh) the
	// custom mount is resolved last and wins (see dirMountSpecsFor). Honored
	// only on backends that report SupportsCustomMounts()==true
	// (devcontainer); other backends silently skip them. Empty (the default)
	// means no custom mounts.
	CustomMounts []string `json:"customMounts,omitempty"`

	// DebCacheServer enables a shared apt-cacher-ng container for this fleet so
	// repeated `apt install` across the fleet's instances reuse downloaded
	// .deb packages instead of re-fetching them. When set, instance
	// provisioning ensures one cache container per fleet, joins every instance
	// to a shared per-fleet docker network, and writes an apt http proxy config
	// (/etc/apt/apt.conf.d/01fleet-proxy) pointing at the cache. Plain bool
	// (default off), matching BuildkitServer.
	//
	// Intent only: honored on SupportsCustomMounts()==true backends; cloud
	// backends silently skip it. Best-effort — instances without apt (or where
	// apt.conf.d is not writable) are skipped without error. Caches only HTTP
	// apt sources (the default Debian/Ubuntu archives); HTTPS sources go direct.
	DebCacheServer bool `json:"debCacheServer,omitempty"`

	// ImageCacheServer enables a shared registry pull-through cache (a
	// `registry:2` mirror of Docker Hub) for this fleet so repeated
	// `docker pull` of docker.io images don't re-download. When set, instance
	// provisioning ensures one cache container per fleet, joins every instance
	// to the shared per-fleet docker network, and points the instance's own
	// dockerd at the mirror (registry-mirrors + insecure-registries in
	// /etc/docker/daemon.json, then a SIGHUP reload). Plain bool (default off).
	//
	// Intent only and best-effort like the others. It only helps instances that
	// run their OWN docker daemon (docker-in-docker); docker-outside-of-docker
	// and no-docker instances are silently skipped. Only docker.io images are
	// mirrored (that is all docker registry-mirrors ever applies to).
	ImageCacheServer bool `json:"imageCacheServer,omitempty"`

	// HomeDir is the absolute path inside the container that should be
	// treated as the home directory when computing mount targets — e.g.
	// "/home/vscode" so a Claude Code mount lands at "/home/vscode/.claude".
	// Empty means "not yet known"; the TUI's edit-fleet dialog tries to
	// auto-detect a value (from devcontainer.json or the image's USER
	// directive) when the user enables a mount, and the resolver falls
	// back to "/home/vscode" if it stays empty at provisioning time.
	HomeDir string `json:"homeDir,omitempty"`

	// LayoutPresets is the fleet's list of saved session-layout templates
	// (issue #150). Each preset pairs a captured outer-tmux pane layout with
	// per-pane startup commands; the new-session dialog Tab-cycles them and
	// applies the selection at creation time. Entries are validated/normalized
	// (see NormalizeLayoutPresets) before persisting. Empty (the default)
	// means no presets.
	LayoutPresets []LayoutPreset `json:"layoutPresets,omitempty"`

	// Agents is the fleet's list of automation agent definitions (issue #188).
	// An Agent describes how an automation worker is launched (command + tmux
	// mode + system prompt + backend); Triggers reference agents by name. The
	// list is validated/normalized (see NormalizeAgents) before persisting.
	// Empty (the default) means no agents. Surfaced in the TUI's automation
	// view.
	Agents []Agent `json:"agents,omitempty"`

	// Triggers is the fleet's list of automation trigger definitions (issue
	// #188). A Trigger fires its referenced Agents (with a prompt) on a
	// schedule or webhook event. Validated/normalized against Agents (see
	// NormalizeTriggers) before persisting. Empty (the default) means no
	// triggers.
	Triggers []Trigger `json:"triggers,omitempty"`

	// PreferFleetLaunch controls which page the built-in browser opens to
	// when a workspace's devcontainer.json configures BOTH a
	// customizations.fleet.browser.initialUrl AND a fleetLaunch block.
	// When only one of the two is configured, it has no effect.
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
