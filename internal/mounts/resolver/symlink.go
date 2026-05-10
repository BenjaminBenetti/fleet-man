package resolver

// Symlink describes a symbolic link the caller must create inside the
// container after the bind mounts in a Resolved are in place. The link
// points from Target to Source; Target is where the link itself lives
// (the path the agent's tooling reads/writes), Source is where the
// real file lives in the container's filesystem (always inside one of
// the bind-mounted directories so writes go through to the host).
//
// Single-file mounts are expressed as Symlinks rather than direct
// bind mounts because many config files are rewritten via "write tmp,
// rename over target" which fails when the target is itself a bind
// mount. Symlinking through a parent-directory mount avoids that
// failure mode entirely.
type Symlink struct {
	// Source is an absolute path inside the container — the file the
	// symlink points to. Must live inside one of the Resolved.Mounts
	// so reads and writes pass through to the host.
	Source string
	// Target is an absolute path inside the container where the
	// symlink is created. Typically a path the agent's CLI looks up
	// by convention (e.g. /home/vscode/.claude.json).
	Target string
	// SeedContent, when non-empty, is written to Source after the
	// symlink is created — but only when Source is still empty after
	// the caller's migration step has run. Used to give brand-new
	// mount targets a valid initial value (e.g. "{}" for Claude
	// Code's ~/.claude.json, which crashes the CLI on install if
	// it parses as anything other than valid JSON). An empty string
	// means no seeding.
	SeedContent string
}
