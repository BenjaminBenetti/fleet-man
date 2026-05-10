// Package startup runs per-fleet "install once at instance creation"
// shell scripts inside a freshly-provisioned container. Each script is
// optional and is selected based on FleetSettings — e.g. enabling the
// Claude Code mount also installs the Claude Code CLI so the mounted
// auth state has a binary to log in with.
//
// Scripts are best-effort: they self-redirect all output to
// ~/.fleet/startup/<name>.log inside the instance and a non-zero exit
// from any single script is surfaced as a warning but never marks the
// instance as failed. Callers can inspect the log file inside the
// container to debug install issues.
package startup

// Script is a single named shell snippet to run inside a container
// after Up. Name is used both as a logical identifier and as the
// log file's basename (~/.fleet/startup/<Name>.log). Body is the raw
// shell content; the runner wraps it with output redirection and exit
// status reporting before execution.
type Script struct {
	// Name identifies the script and is used as the log filename.
	// Must be a shell-safe identifier (kebab-case ASCII).
	Name string

	// Body is the shell snippet to execute. It runs under /bin/sh and
	// inherits the container's environment for the workspace user.
	Body string
}
