//go:build !linux

package podseed

import (
	"context"
	"fmt"
	"io"

	"github.com/BenjaminBenetti/fleet-man/internal/jobboot"
)

// RunJob fails off linux: Jobs run in linux pods.
func RunJob(_ context.Context, _ string, log io.Writer) int {
	fmt.Fprintln(log, "fleet pod-job: pod jobs run only inside a linux pod")
	return jobboot.ExitFailed
}
