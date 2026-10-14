package backend

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// JobReporter is the part of a job's provisioning-log sink (WithJobLog) that
// goes beyond raw output: it records the step the run has entered and the
// non-fatal warnings it hits as events of their own, so a watcher can show the
// step and tell a warning from output. The daemon's provisioning log implements
// it. Code reports through ReportStep and ReportWarning, never by asserting
// the interface itself.
type JobReporter interface {
	// Step records that the run entered step; detail, when not empty, says
	// more (the command, the image).
	Step(step fleet.JobStep, detail string)
	// Warn records a non-fatal warning (it may span lines).
	Warn(message string)
}

// ReportStep tells ctx's provisioning log that the run entered step. A sink that
// is not a JobReporter gets the step as a plain "==> label" line; with no sink
// attached it does nothing.
func ReportStep(ctx context.Context, step fleet.JobStep, detail string) {
	w := JobLogFrom(ctx)
	if r, ok := w.(JobReporter); ok {
		r.Step(step, detail)
		return
	}
	if w != io.Discard {
		_, _ = fmt.Fprintf(w, "==> %s\n", StepLine(step, detail))
	}
}

// ReportWarning tells ctx's provisioning log about a non-fatal warning. A sink
// that is not a JobReporter gets it as "warning: " lines; with no sink attached
// it does nothing. It does not replace state.WriteWarn: the instance's warning
// banner is written there, this only adds the warning to the log.
func ReportWarning(ctx context.Context, message string) {
	w := JobLogFrom(ctx)
	if r, ok := w.(JobReporter); ok {
		r.Warn(message)
		return
	}
	if w != io.Discard {
		for _, line := range strings.Split(strings.TrimRight(message, "\n"), "\n") {
			_, _ = fmt.Fprintf(w, "warning: %s\n", line)
		}
	}
}

// StepLine is how a step reads in a log: its label, then the detail.
func StepLine(step fleet.JobStep, detail string) string {
	label := step.Label()
	switch {
	case label == "":
		return detail
	case detail == "":
		return label
	default:
		return label + ": " + detail
	}
}
