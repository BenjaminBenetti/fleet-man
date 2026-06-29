// Package fleetlaunch manages the fleet-launch binary inside a
// devcontainer — the fleet executable that the host stages into an
// instance so its in-container subcommands (the landing-page server
// today, more to come) can be invoked there.
//
// The host stages the binary at two points: on instance start (so it's
// ready by the time anything reaches for it) and on browser open (so a
// long-lived workspace picks up host-side fleet updates). Both flow
// through EnsureFresh, which only writes when the remote copy is
// missing or stale — meaning the second call is cheap and idempotent.
package fleetlaunch

import (
	"fmt"
	"os"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/version"
)

// RemotePath is the absolute path inside the container where the host
// stages the fleet binary. /usr/bin so it's on $PATH — anything inside
// the container (interactive shells, build scripts, future helpers
// invoked from agents) can run `fleet …` without knowing where the
// binary lives. Fixed because each container hosts exactly one
// install. remoteDir is the parent directory; the copy script probes
// it for writability to decide whether to use sudo.
const (
	RemotePath = "/usr/bin/fleet"
	remoteDir  = "/usr/bin"
)

// EnsureFresh stages the host's fleet binary at RemotePath when the
// remote copy is absent, predates the version subcommand, is a dev
// build, or has a different version than the host. A host that is
// itself a dev build always re-stages, so host-side iteration
// propagates into long-lived workspaces. Returns whether the binary
// was actually copied.
//
// beforeRefresh, if non-nil, runs immediately before the overwrite when
// EnsureFresh decides to refresh — callers use it to stop any
// in-container process that has the old binary mmap'd, so the new code
// reliably takes effect on the next start. It is NOT called when the
// binary is already up to date, so passing a hook that always kills a
// service won't churn services unnecessarily.
func EnsureFresh(instanceBackend backend.Backend, workspaceDir string, beforeRefresh func() error) (bool, error) {
	refresh, err := needsRefresh(instanceBackend, workspaceDir)
	if err != nil {
		return false, err
	}
	if !refresh {
		return false, nil
	}
	if beforeRefresh != nil {
		if err := beforeRefresh(); err != nil {
			return false, err
		}
	}
	if err := copyBinary(instanceBackend, workspaceDir); err != nil {
		return false, err
	}
	return true, nil
}

// needsRefresh reports whether the binary at RemotePath should be
// replaced with the host's. It runs the remote binary's `version`
// subcommand and classifies the outcome into four states — absent,
// pre-version-command (old binary that doesn't know the subcommand),
// dev build (no version baked in), or a real version string — then
// compares that against the host:
//
//   - host is a dev build           → always refresh (so host-side
//     iteration propagates into long-lived workspaces).
//   - remote isn't a real version   → refresh (absent / old / dev).
//   - remote version != host version → refresh.
//   - otherwise                     → leave it alone.
//
// The four states are signalled by stdout markers from a single shell
// snippet so the whole probe is one exec call.
func needsRefresh(instanceBackend backend.Backend, workspaceDir string) (bool, error) {
	if version.Version == "" {
		return true, nil
	}

	probe := fmt.Sprintf(`
if [ ! -x %s ]; then
  echo ABSENT
elif ! out="$(%s version 2>/dev/null)"; then
  echo NO_VERSION_CMD
else
  ver="$(printf '%%s' "$out" | tr -d '[:space:]')"
  if [ -z "$ver" ]; then
    echo DEV
  else
    echo "VERSION $ver"
  fi
fi`, RemotePath, RemotePath)

	out, err := instanceBackend.ExecCommand(workspaceDir, []string{"sh", "-c", probe}).CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("probe remote binary: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	line := strings.TrimSpace(string(out))
	switch {
	case line == "ABSENT", line == "NO_VERSION_CMD", line == "DEV":
		return true, nil
	case strings.HasPrefix(line, "VERSION "):
		return strings.TrimPrefix(line, "VERSION ") != version.Version, nil
	default:
		// Unexpected output — be conservative and replace the binary so
		// we converge on a known-good state.
		return true, nil
	}
}

// stagePath is the user-writable path the binary is transferred to before being
// installed into the privileged RemotePath. /tmp is writable without sudo on
// every backend, so CopyFile (which is itself sudo-free) can always land it here.
const stagePath = "/tmp/.fleet-launch-stage"

// copyBinary transfers the running fleet binary into the container at RemotePath
// in two stdin-EOF-safe steps:
//
//   - CopyFile streams it to a user-writable staging path. CopyFile is the
//     backend strategy seam, so this works on every backend including coder
//     (whose `coder ssh` transport hangs a stdin-reading `cat`); the host-side
//     conditional that used to branch on transport is gone.
//
//   - a small exec installs it into RemotePath. /usr/bin is typically not
//     writable by a devcontainer's default user, so the script probes the
//     directory and either moves directly or re-runs through `sudo -n`
//     (passwordless, matching the privoxy/tmux installers elsewhere). It unlinks
//     the old file before the move: a process the caller just signalled may
//     still have the old binary mmap'd, and overwriting a mapped executable
//     surfaces as ETXTBSY — rm-then-mv allocates a fresh inode so the running
//     process keeps its old copy. The install reads no stdin, so it cannot hang
//     on any backend.
func copyBinary(instanceBackend backend.Backend, workspaceDir string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate fleet binary: %w", err)
	}
	bin, err := os.Open(self)
	if err != nil {
		return fmt.Errorf("open fleet binary: %w", err)
	}
	defer bin.Close()

	if err := instanceBackend.CopyFile(workspaceDir, bin, stagePath, 0o755); err != nil {
		return fmt.Errorf("stage fleet binary: %w", err)
	}

	install := fmt.Sprintf(`
write='rm -f %[1]s && mv -f %[2]s %[1]s && chmod +x %[1]s'
if [ -w %[3]s ]; then
  sh -c "$write"
else
  sudo -n sh -c "$write"
fi`, RemotePath, stagePath, remoteDir)

	if out, err := instanceBackend.ExecCommand(workspaceDir, []string{"sh", "-c", install}).CombinedOutputWithTimeout(backend.CopyTimeout); err != nil {
		return fmt.Errorf("install fleet binary: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}
