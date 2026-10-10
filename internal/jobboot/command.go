package jobboot

import (
	"fmt"
	"path"
	"strconv"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/shellquote"
)

// The tools dir is an emptyDir every Job pod mounts: fleetd stages the binary
// and the spec into it, and pod-job listens on its socket.
const (
	// ToolsDir is where a Job pod mounts its tools emptyDir.
	ToolsDir = "/fleet-tools"
	// ReadyName is the sentinel the wait loop polls for; fleetd writes it
	// last, once the binary and the spec are both in place.
	ReadyName = ".ready"
	// BinaryName is the staged fleet-pod binary.
	BinaryName = "fleet"
	// SpecName is the staged job spec, `pod-job --spec`'s argument.
	SpecName = "job.json"
	// SocketName is the unix socket pod-job listens on for its feed.
	SocketName = "job.sock"
)

// Exit codes a Job's container ends with, beyond 0 (done) and the job's own
// failure (ExitFailed). They follow sysexits.h, so `kubectl get pod` reads as
// a reason.
const (
	// ExitFailed: the job ran and failed; the termination message says why.
	ExitFailed = 1
	// ExitNotStaged (EX_SOFTWARE): the wait loop gave up; fleetd never staged
	// the binary and spec.
	ExitNotStaged = 70
	// ExitStalled (EX_TEMPFAIL): the job waited StallTimeout for fleetd
	// without receiving a byte.
	ExitStalled = 75
	// ExitBadSpec (EX_CONFIG): the spec did not parse or validate, or the
	// running binary is not the one the spec names.
	ExitBadSpec = 78
)

// Defaults for the waits; the spec and the renderer may override each.
const (
	// DefaultWaitSeconds is how long the wait loop polls for .ready.
	DefaultWaitSeconds = 600
	// DefaultStallTimeout is how long a job waits on fleetd without a byte.
	DefaultStallTimeout = 5 * time.Minute
	// DefaultResultHold is how long a finished job keeps its container up
	// for fleetd to collect and acknowledge its result.
	DefaultResultHold = 2 * time.Minute
)

// Paths is where a Job pod's tools live.
type Paths struct {
	// Dir is the tools emptyDir's mount path; "" is ToolsDir.
	Dir string
}

func (p Paths) dir() string {
	if p.Dir == "" {
		return ToolsDir
	}
	return p.Dir
}

// Binary is the staged fleet binary's path.
func (p Paths) Binary() string { return path.Join(p.dir(), BinaryName) }

// Spec is the staged job spec's path.
func (p Paths) Spec() string { return path.Join(p.dir(), SpecName) }

// Ready is the sentinel's path.
func (p Paths) Ready() string { return path.Join(p.dir(), ReadyName) }

// Socket is pod-job's socket path.
func (p Paths) Socket() string { return path.Join(p.dir(), SocketName) }

// Command is a Job container's command: the shell loop that waits up to
// waitSeconds (0 is DefaultWaitSeconds) for the sentinel, then execs pod-job on
// the staged spec. It uses only POSIX sh, so it runs in any image.
func Command(p Paths, waitSeconds int) []string {
	if waitSeconds <= 0 {
		waitSeconds = DefaultWaitSeconds
	}
	script := fmt.Sprintf(
		"i=0; until [ -f %s ]; do i=$((i+1)); [ $i -gt %d ] && exit %d; sleep 1; done; exec %s pod-job --spec %s",
		shellquote.Single(p.Ready()), waitSeconds, ExitNotStaged,
		shellquote.Single(p.Binary()), shellquote.Single(p.Spec()))
	return []string{"sh", "-c", script}
}

// itoa is strconv.Itoa for the int64 sizes the scripts carry.
func itoa(n int64) string { return strconv.FormatInt(n, 10) }
