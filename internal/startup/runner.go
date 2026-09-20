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
	cmd := instanceBackend.ExecCommand(wsDir, []string{"sh", "-c", wrap(script)})
	out, err := cmd.CombinedOutputWithTimeout(timeout) // 0 = no deadline
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
func wrap(script Script) string {
	return fmt.Sprintf(`mkdir -p %[1]s
exec 3>&2
exec >>%[1]s/%[2]s.log 2>&1
echo "=== %[2]s @ $(date -u +%%FT%%TZ) ==="
start=$(wc -l < %[1]s/%[2]s.log 2>/dev/null || echo 0)
(
%[3]s
)
rc=$?
if [ "$rc" -ne 0 ]; then
  tail -n +$((start + 1)) %[1]s/%[2]s.log | tail -n %[4]d >&3
fi
exit "$rc"
`,
		logDir, script.Name, script.Body, failureTailLines,
	)
}
