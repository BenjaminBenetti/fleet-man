package create

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

var (
	// dotfilesInstallTimeout bounds a single dotfiles-install attempt. A dotfiles
	// install can hang indefinitely — a `git clone` stuck on an unreachable host,
	// an install script blocked on input — and without a bound that hang stalls
	// the entire instance creation. Overridable via FLEET_DOTFILES_INSTALL_TIMEOUT
	// (a Go duration) so tests run in milliseconds.
	dotfilesInstallTimeout = envDurationDefault("FLEET_DOTFILES_INSTALL_TIMEOUT", 2*time.Minute)

	// dotfilesInstallAttempts is how many times the install is tried before giving
	// up and starting the instance anyway. Each attempt is bounded by
	// dotfilesInstallTimeout. Overridable via FLEET_DOTFILES_INSTALL_ATTEMPTS.
	dotfilesInstallAttempts = envIntDefault("FLEET_DOTFILES_INSTALL_ATTEMPTS", 3)
)

// dotfilesExecer is the slice of backend.Backend installDotfiles needs: build a
// command to run inside the workspace. Narrowing to this interface keeps the
// retry logic unit-testable with a tiny fake instead of a full Backend.
type dotfilesExecer interface {
	ExecCommand(workspaceDir string, command []string) *backend.Cmd
}

// installDotfiles runs the dotfiles setup script inside the instance, bounded
// and best-effort: each attempt is killed if it overruns dotfilesInstallTimeout
// and the install is retried up to dotfilesInstallAttempts times. If every
// attempt fails (or hangs), the failure is surfaced as a single warning and the
// caller carries on — a broken dotfiles install must never block the instance
// from coming up.
//
// On retries the script is prefixed with `rm -rf ~/dotfiles` because a killed
// attempt can leave a partial clone behind, and dotfiles.SetupScript's
// `[ ! -d ~/dotfiles ]` guard would then skip the reinstall — making the retry
// a silent no-op. Clearing it first ensures each retry genuinely re-runs the
// clone + install. This only ever fires after a failed attempt, so a legitimate
// pre-existing ~/dotfiles (which makes attempt 1 a successful no-op) is never
// touched.
func installDotfiles(execer dotfilesExecer, fleetName, instanceName, wsDir, script string) {
	var lastErr error
	var lastOut string
	for attempt := 1; attempt <= dotfilesInstallAttempts; attempt++ {
		runScript := script
		if attempt > 1 {
			runScript = "rm -rf ~/dotfiles; " + script
		}
		cmd := execer.ExecCommand(wsDir, []string{"sh", "-c", runScript})
		out, err := cmd.CombinedOutputWithTimeout(dotfilesInstallTimeout)
		if err == nil {
			return
		}
		lastErr, lastOut = err, strings.TrimSpace(string(out))
	}

	// All attempts failed. WriteWarn overwrites the per-instance warn file, so we
	// emit a single summary rather than one warning per attempt.
	state.WriteWarn(fleetName, instanceName, fmt.Sprintf(
		"dotfiles install failed after %d attempt(s); instance started anyway: %v\n%s",
		dotfilesInstallAttempts, lastErr, lastOut))
}

// envDurationDefault returns the Go duration parsed from the named env var, or
// def when it is unset, blank, or unparseable (a non-positive value is also
// rejected). The override exists so tests can shrink the dotfiles timeout to
// milliseconds; production uses the defaults.
func envDurationDefault(name string, def time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

// envIntDefault returns the positive integer parsed from the named env var, or
// def when it is unset, blank, non-numeric, or non-positive.
func envIntDefault(name string, def int) int {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return def
}
