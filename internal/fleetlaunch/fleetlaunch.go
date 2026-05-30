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

// copyBinary streams the running fleet binary into the container at
// RemotePath. It unlinks the old file first rather than truncating it:
// a process the caller has just signalled may still have the old
// binary mmap'd, and overwriting a mapped executable surfaces as
// ETXTBSY. Unlink-then-create allocates a fresh inode so the running
// process keeps its old copy and the new file is written cleanly.
//
// /usr/bin is typically not writable by a devcontainer's default user,
// so the script probes the directory once and either writes directly or
// re-runs through `sudo -n` (passwordless, matching the pattern used by
// privoxy and tmux installers elsewhere). The branch is decided BEFORE
// stdin is consumed so the binary bytes flow down exactly one of the
// two paths — important because a pipe can only be read once.
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

	script := fmt.Sprintf(`
write='rm -f %s && cat > %s && chmod +x %s'
if [ -w %s ]; then
  sh -c "$write"
else
  sudo -n sh -c "$write"
fi`, RemotePath, RemotePath, RemotePath, remoteDir)

	cmd := instanceBackend.ExecCommand(workspaceDir, []string{"sh", "-c", script})
	cmd.Stdin = bin
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("copy fleet binary: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}
