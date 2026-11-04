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
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"strings"
	"sync/atomic"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/binstore"
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

// MicSinkArgv is the in-instance command line of the virtual microphone's
// sink (`fleet mic sink`). The staged binary is named by its absolute path, so
// the command does not depend on the exec user's PATH.
func MicSinkArgv() []string { return []string{RemotePath, "mic", "sink"} }

// OutputSourceArgv is the in-instance command line of the audio output's
// source (`fleet output source`), by the staged binary's absolute path.
func OutputSourceArgv() []string { return []string{RemotePath, "output", "source"} }

// EnsureFresh stages a matching full Linux fleet binary at RemotePath when the
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
//
// A backend that stages the binary its own way (Stager) does all of this
// itself.
func EnsureFresh(instanceBackend backend.Backend, workspaceDir string, beforeRefresh func() error) (bool, error) {
	return ensureFresh(instanceBackend, workspaceDir, beforeRefresh, func(arch string) (*binstore.Artifact, error) {
		return binstore.Default().FullBinary(context.Background(), arch)
	})
}

func ensureFresh(instanceBackend backend.Backend, workspaceDir string, beforeRefresh func() error, resolve func(string) (*binstore.Artifact, error)) (bool, error) {
	if stager, ok := instanceBackend.(Stager); ok {
		return stager.StageFleet(workspaceDir, beforeRefresh)
	}
	refresh, err := needsRefresh(instanceBackend, workspaceDir)
	if err != nil {
		return false, err
	}
	if !refresh {
		return false, nil
	}
	arch, err := containerArch(instanceBackend, workspaceDir)
	if err != nil {
		return false, err
	}
	artifact, err := resolve(arch)
	if err != nil {
		return false, fmt.Errorf("resolve fleet binary for linux/%s: %w", arch, err)
	}
	bin, err := artifact.Open()
	if err != nil {
		return false, fmt.Errorf("open fleet binary: %w", err)
	}
	defer bin.Close()
	if err := copyBinary(instanceBackend, workspaceDir, bin, beforeRefresh); err != nil {
		return false, err
	}
	return true, nil
}

// containerArch probes the instance rather than assuming the daemon's CPU or
// Docker host architecture. This also handles emulated and remote containers.
func containerArch(b backend.Backend, workspaceDir string) (string, error) {
	out, err := b.ExecCommand(workspaceDir, []string{"sh", "-c", "uname -s && uname -m"}).CombinedOutputWithTimeout(backend.CopyTimeout)
	if err != nil {
		return "", fmt.Errorf("probe container architecture: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	fields := strings.Fields(string(out))
	if len(fields) == 2 && fields[0] == "Linux" {
		switch fields[1] {
		case "x86_64", "amd64":
			return "amd64", nil
		case "aarch64", "arm64":
			return "arm64", nil
		}
	}
	return "", fmt.Errorf("unsupported container platform %q; need Linux amd64 or arm64", strings.TrimSpace(string(out)))
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

	out, err := instanceBackend.ExecCommand(workspaceDir, []string{"sh", "-c", probe}).CombinedOutputWithTimeout(backend.CopyTimeout)
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

// stageSeq makes stageBinaryPath unique even if crypto/rand.Read fails (leaving
// the byte buffer zeroed), so two concurrent stagers can never collide — the same
// belt-and-suspenders the coder backend's remoteStageName uses.
var stageSeq atomic.Uint64

// stageBinaryPath returns a per-call-unique, user-writable path the binary is
// transferred to before being installed into the privileged RemotePath. /tmp is
// writable without sudo on every backend, so CopyFile (which is itself sudo-free)
// can always land it here. The seq+random suffix keeps two concurrent stagers
// from clobbering each other's staging file between CopyFile and the install mv,
// matching the per-call uniqueness used elsewhere in the staging path.
func stageBinaryPath() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("/tmp/.fleet-launch-stage.%d.%x", stageSeq.Add(1), b[:])
}

// inRemoteDir runs a filesystem operation with the privileges needed to write
// /usr/bin, using the same check for preparation, installation and cleanup.
func inRemoteDir(command string) string {
	return fmt.Sprintf(`
if [ -w %s ]; then
  sh -c %s
else
  sudo -n sh -c %s
fi`, remoteDir, backend.ShellQuote(command), backend.ShellQuote(command))
}

// copyBinary transfers through the backend's stdin-EOF-safe CopyFile into /tmp,
// then moves the candidate beside RemotePath before executing it: /tmp may be
// mounted noexec. Preparation and verification leave the installed binary and
// its services untouched. After beforeRefresh, a same-directory rename replaces
// the old inode atomically, even if a process still has it mmap'd.
func copyBinary(instanceBackend backend.Backend, workspaceDir string, bin io.Reader, beforeRefresh func() error) error {
	stagePath := stageBinaryPath()
	candidate := remoteDir + strings.TrimPrefix(stagePath, "/tmp")
	installed := false
	defer func() {
		if installed {
			return
		}
		// Always remove the unprivileged transfer, even when sudo is denied.
		// A failed cross-filesystem move may also leave a partial candidate.
		cleanup := fmt.Sprintf("rm -f %s\nif [ -e %s ]; then\n%s\nfi",
			backend.ShellQuote(stagePath), backend.ShellQuote(candidate),
			inRemoteDir("rm -f "+backend.ShellQuote(candidate)))
		_, _ = instanceBackend.ExecCommand(workspaceDir, []string{"sh", "-c", cleanup}).CombinedOutputWithTimeout(backend.CopyTimeout)
	}()
	if err := instanceBackend.CopyFile(workspaceDir, bin, stagePath, 0o755); err != nil {
		return fmt.Errorf("stage fleet binary: %w", err)
	}
	prepare := inRemoteDir(fmt.Sprintf("mv -f %s %s && chmod 0755 %s", stagePath, candidate, candidate))
	if out, err := instanceBackend.ExecCommand(workspaceDir, []string{"sh", "-c", prepare}).CombinedOutputWithTimeout(backend.CopyTimeout); err != nil {
		return fmt.Errorf("prepare fleet binary: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	out, err := instanceBackend.ExecCommand(workspaceDir, []string{candidate, "version"}).CombinedOutputWithTimeout(backend.CopyTimeout)
	if err != nil {
		return fmt.Errorf("verify staged fleet binary: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	if got := strings.TrimSpace(string(out)); got != version.Version {
		return fmt.Errorf("staged fleet version %q does not match host %q", got, version.Version)
	}
	if beforeRefresh != nil {
		if err := beforeRefresh(); err != nil {
			return err
		}
	}
	install := inRemoteDir(fmt.Sprintf("mv -f %s %s", candidate, RemotePath))
	if out, err := instanceBackend.ExecCommand(workspaceDir, []string{"sh", "-c", install}).CombinedOutputWithTimeout(backend.CopyTimeout); err != nil {
		return fmt.Errorf("install fleet binary: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	installed = true
	return nil
}
