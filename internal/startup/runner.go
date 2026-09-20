package startup

import (
	"fmt"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// ===========================================
// Constants
// ===========================================

// logDir is the directory inside the container where each script's
// stdout/stderr is captured. Lives under ~/.fleet so it is namespaced
// to the workspace user and survives whatever the install does to its
// own working directory.
const logDir = "~/.fleet/startup"

// ===========================================
// Public API
// ===========================================

// Run executes each script in order inside the just-provisioned
// container. Every script's stdout/stderr is redirected to
// ~/.fleet/startup/<name>.log inside the container before its body
// runs, so script output never reaches the host.
//
// A failure in any single script is returned in the result slice but
// does not stop subsequent scripts from running. Callers must treat
// the returned errors as warnings — failing the instance for a missed
// agent install is intentionally avoided so the user still gets a
// working container.
func Run(instanceBackend backend.Backend, wsDir string, scripts []Script) []Error {
	return RunWithTimeout(instanceBackend, wsDir, scripts, 0)
}

// RunWithTimeout is Run with a deadline PER SCRIPT (0 = none). Provisioning runs
// its scripts once, in line, with a human watching; a background caller — the
// daemon installing the microphone's packages into an instance that predates
// the feature — must not be held forever by an apt that hangs on an unreachable
// mirror.
func RunWithTimeout(instanceBackend backend.Backend, wsDir string, scripts []Script, timeout time.Duration) []Error {
	if len(scripts) == 0 {
		return nil
	}
	var errs []Error
	for _, script := range scripts {
		if err := runOne(instanceBackend, wsDir, script, timeout); err != nil {
			errs = append(errs, Error{
				ScriptName: script.Name,
				LogPath:    fmt.Sprintf("%s/%s.log", logDir, script.Name),
				Err:        err,
			})
		}
	}
	return errs
}

// ===========================================
// Internal helpers
// ===========================================

// runOne executes a single script inside the container via the
// backend's ExecCommand. The script body is wrapped with output
// redirection so the host-side combined output is essentially empty;
// the meaningful logs live inside the container at logDir/<name>.log.
func runOne(instanceBackend backend.Backend, wsDir string, script Script, timeout time.Duration) error {
	cmd := instanceBackend.ExecCommand(wsDir, boundedShell(wrap(script), timeout))
	out, err := cmd.CombinedOutputWithTimeout(hostDeadline(timeout))
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail != "" {
			return fmt.Errorf("%w: %s", err, detail)
		}
		return err
	}
	return nil
}

// failureTailLines is how much of a failed script's log is echoed back to the
// host. Enough for a script's closing diagnosis; short enough for a banner.
const failureTailLines = 6

// wrap returns a shell snippet that ensures the log directory exists,
// redirects all subsequent output to the script's log file, prints a
// header for the run, and then executes the script body. The body's
// exit code becomes the wrapper's exit code, which the runner observes
// via *exec.Cmd.CombinedOutput.
//
// On FAILURE the tail of THIS RUN's output (the log is appended to, so the
// line count is noted before the body starts — a short failing run must not be
// padded out with the previous run's lines, which would also make every retry's
// error text different) is echoed to the original stderr, so the error
// the host sees — and the warning the user gets — says WHY, not just "exit
// status 3" with the explanation left in a file inside the container. The body
// runs in a subshell for that: a script is free to `exit` (or set its own EXIT
// trap) without taking the wrapper down with it.
//
// fd 3 — the wrapper's handle on the host's stderr — is CLOSED for the body
// (`3>&-`). The host side reads the exec's output until every writer has closed
// it, so anything the body leaves running (a daemon it starts; the mic script
// starts a sound server) would otherwise inherit fd 3 and hang the caller for as
// long as it lives.
func wrap(script Script) string {
	return fmt.Sprintf(`mkdir -p %[1]s
exec 3>&2
exec >>%[1]s/%[2]s.log 2>&1
echo "=== %[2]s @ $(date -u +%%FT%%TZ) ==="
start=$(wc -l < %[1]s/%[2]s.log 2>/dev/null || echo 0)
(
%[3]s
) 3>&-
rc=$?
if [ "$rc" -ne 0 ]; then
  tail -n +$((start + 1)) %[1]s/%[2]s.log | tail -n %[4]d >&3
fi
exit "$rc"
`,
		logDir, script.Name, script.Body, failureTailLines,
	)
}

// boundedShell is the argv that runs script inside the instance. A deadline has
// to be enforced THERE as well as on the host: killing the host-side exec does
// not stop what is running in the container, and an apt left behind would keep
// the dpkg lock — so the next attempt (and the user's own apt) would fail. With
// a timeout the script runs under coreutils/busybox `timeout` when the instance
// has one (-k: SIGKILL if it ignores SIGTERM); without one it runs unbounded, as
// it always did, and the host deadline is the only limit.
func boundedShell(script string, timeout time.Duration) []string {
	if timeout <= 0 {
		return []string{"sh", "-c", script}
	}
	seconds := int(timeout / time.Second)
	launcher := fmt.Sprintf(`if command -v timeout >/dev/null 2>&1; then exec timeout -k 10 %d sh -c "$0"; else exec sh -c "$0"; fi`, seconds)
	return []string{"sh", "-c", launcher, script}
}

// hostDeadline gives the in-instance deadline room to fire first (so the script
// is stopped cleanly, inside the container), and only then cuts the exec.
func hostDeadline(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return 0
	}
	return timeout + 30*time.Second
}
