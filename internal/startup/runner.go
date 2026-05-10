package startup

import (
	"fmt"
	"strings"

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
	if len(scripts) == 0 {
		return nil
	}
	var errs []Error
	for _, script := range scripts {
		if err := runOne(instanceBackend, wsDir, script); err != nil {
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
func runOne(instanceBackend backend.Backend, wsDir string, script Script) error {
	cmd := instanceBackend.ExecCommand(wsDir, []string{"sh", "-c", wrap(script)})
	out, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail != "" {
			return fmt.Errorf("%w: %s", err, detail)
		}
		return err
	}
	return nil
}

// wrap returns a shell snippet that ensures the log directory exists,
// redirects all subsequent output to the script's log file, prints a
// header for the run, and then executes the script body. The body's
// exit code becomes the wrapper's exit code, which the runner observes
// via *exec.Cmd.CombinedOutput.
func wrap(script Script) string {
	return fmt.Sprintf(`mkdir -p %s
exec >>%s/%s.log 2>&1
echo "=== %s @ $(date -u +%%FT%%TZ) ==="
%s
`,
		logDir,
		logDir, script.Name,
		script.Name,
		script.Body,
	)
}
