package syncpolicy

// Defaults returns the fleet sync policy table.
//
// Adding an agent tool (a new FleetSettings.*Mount toggle with a directory
// mount in internal/mounts/resolver) means adding its Root here, Tool named
// as the resolver names the mount, with a Credential rule for its login
// file, plus the shared agentLocal rules. TestSyncPolicyCoversEveryMount
// fails until it is here.
func Defaults() Table {
	return Table{roots: defaultRoots()}
}

// Size units for the caps below.
const (
	mib int64 = 1 << 20
	gib int64 = 1 << 30
)

// agentLocal are the paths every agent tool's state folder keeps per
// location: installer downloads (two locations installing at once is the
// #149 race), live sessions and daemons, package caches and lock files.
var agentLocal = []Rule{
	{Pattern: "downloads/**", Policy: LocationLocal},
	{Pattern: "sessions/**", Policy: LocationLocal},
	{Pattern: "daemon/**", Policy: LocationLocal},
	{Pattern: "packages/**", Policy: LocationLocal},
	{Pattern: "**/*.lock", Policy: LocationLocal},
}

func defaultRoots() []Root {
	return []Root{
		{
			Path: ".claude",
			Tool: "Claude Code",
			Rules: agentRules(
				Rule{Pattern: ".credentials.json", Policy: Credential},
				Rule{Pattern: "settings.json", Policy: JSON3},
				Rule{Pattern: "history.jsonl", Policy: AppendUnion},
				Rule{Pattern: "projects/**", Policy: Mirror},
			),
			Default: Mirror,
			// Transcripts grow without bound; the share starts at 10Gi.
			Cap:      8 * gib,
			Warnings: []SizeWarning{{Pattern: "projects/**", Size: 2 * gib}},
		},
		{
			Path: ".codex",
			Tool: "Codex",
			Rules: agentRules(
				Rule{Pattern: "auth.json", Policy: Credential},
				Rule{Pattern: "config.toml", Policy: TOML3},
				Rule{Pattern: "history.jsonl", Policy: AppendUnion},
				// Codex's live SQLite state (and its -wal/-shm) and its
				// app-server sockets only work on one kernel.
				Rule{Pattern: "**/*.sqlite*", Policy: LocationLocal},
				Rule{Pattern: "**/app-server-*", Policy: LocationLocal},
			),
			Default: Mirror,
			Cap:     4 * gib,
		},
		{
			Path: ".config/gh",
			Tool: "GitHub CLI",
			// hosts.yml holds the oauth token; gh tokens do not rotate, but gh
			// stays per location with the agent logins.
			Rules:   agentRules(Rule{Pattern: "hosts.yml", Policy: Credential}),
			Default: Mirror,
			Cap:     64 * mib,
		},
		{
			Path:    ".augment",
			Tool:    "Auggie",
			Rules:   agentRules(Rule{Pattern: "session.json", Policy: Credential}),
			Default: Mirror,
			Cap:     2 * gib,
		},
		{
			// The single-file mounts, seeded '{}' by every new instance.
			Path: "files",
			Rules: []Rule{
				{Pattern: ".claude.json", Policy: JSON3},
				{Pattern: "claude-settings.local.json", Policy: JSON3},
			},
			Default: Mirror,
			Cap:     64 * mib,
		},
		{
			// Custom mounts hold user content: mirrored, lock files included
			// (yarn.lock, Cargo.lock), with no size cap, as on local Docker,
			// where only the host disk limits them.
			Path:    ".mnt",
			Default: Mirror,
		},
		{
			// Live host binds (~/.aws, ~/.gitconfig), one folder per bind:
			// user content like .mnt, so no size cap either.
			Path:    ".hostbind",
			Default: Mirror,
		},
	}
}

// agentRules returns a tool's own rules followed by agentLocal. The tool's
// come first, so its login and settings files are never taken for one of
// the shared per-location paths.
func agentRules(own ...Rule) []Rule {
	return append(own, agentLocal...)
}
