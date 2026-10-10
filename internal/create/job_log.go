package create

import (
	"context"
	"io"
	"os"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// job_log.go connects a provisioning run to its job's provisioning log, the
// sink the daemon attaches to ctx (backend.WithJobLog): the instance's creation
// log file plus the job's JobLog / JobProgress events. Each step the run enters
// is reported with backend.ReportStep, the commands it runs on the host write
// their output there (stepOutput), and every non-fatal warning lands there as
// well as on the instance's banner (provisionWarning). With no log attached
// all of this is a no-op beyond what the run did before.

// provisionWarning records a non-fatal provisioning warning: the instance's
// warning banner (state.WriteWarn, which the TUI shows once it is running) and
// a warning in the job's provisioning log.
func provisionWarning(ctx context.Context, fleetName, instanceName, message string) {
	state.WriteWarn(fleetName, instanceName, message)
	backend.ReportWarning(ctx, message)
}

// stepOutput is where a host command a step runs writes its output: the
// process's stderr, as before, and the job's provisioning log.
func stepOutput(ctx context.Context) io.Writer {
	return io.MultiWriter(os.Stderr, backend.JobLogFrom(ctx))
}

// prependBackendWarnings puts an Up's warnings (UpResult.Warnings) in front
// of the instance's banner, which the best-effort provisioning steps replace
// as they warn: they stay in view, with the steps' warnings kept below them.
func prependBackendWarnings(fleetName, instanceName string, warnings []string) {
	var lines []string
	for _, w := range warnings {
		if w = strings.TrimSpace(w); w != "" {
			lines = append(lines, w)
		}
	}
	if len(lines) > 0 {
		state.PrependWarn(fleetName, instanceName, strings.Join(lines, "\n"))
	}
}
