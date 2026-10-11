package backendtest

import (
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// Target is one backend the suite runs against.
type Target struct {
	// Name labels the run in logs: "devcontainer", "kubernetes:minikube",
	// "stub".
	Name string

	// Type is the backend type on the instance records the suite makes.
	Type fleet.BackendType

	// Bind returns the backend that acts on inst, the way the daemon binds one
	// (backendutil.NewForInstance). The suite binds the CREATING record (no
	// ContainerID) to create, and binds again whenever an operation gives the
	// record a new ContainerID.
	Bind func(inst *fleet.Instance) backend.Backend

	// Capabilities, when set, returns the capability-only handle for inst
	// (backendutil.CapabilitiesFor); the capabilities case checks it answers
	// like the bound backend.
	Capabilities func(inst *fleet.Instance) backend.Capabilities

	// Leftovers lists what the backend still holds for inst: containers,
	// volumes, committed images, cluster objects. After every teardown the
	// suite requires it to come back empty.
	Leftovers func(inst *fleet.Instance) ([]string, error)

	// HostProcesses says instance commands run as processes on this host
	// (Stub). The fixture image's tools (sudo, dpkg, tmux, socat) are then not
	// guaranteed, and a process scan sees the host's processes: the checks
	// that need them skip, saying so. On every other target those tools are
	// part of the fixture and a missing one fails.
	HostProcesses bool

	// EventLog is the file the backend's ExecCommand writes its "container
	// exec" entries to (flog.Path() under the test's HOME). Empty skips the
	// event-log half of the exec-quiet case.
	EventLog string

	// Sizes are the bulk transfer sizes; the zero value means PlanSizes.
	Sizes Sizes

	// ListSessionsP95 bounds the 95th percentile of ListSessions' latency on
	// the shared instance. Zero only measures and logs it.
	ListSessionsP95 time.Duration

	// RebuildRollsBack says a rebuild keeps the instance's previous
	// container (generation) until the new one runs, and puts it back when
	// the rebuild fails (Kubernetes). Docker's rebuild removes the old
	// container first, so the rebuild-rollback case skips elsewhere.
	RebuildRollsBack bool

	// Skip names cases this target cannot run yet (by Case.Name), each with
	// the reason the case prints when it skips.
	Skip map[string]string

	// CaseTimeout bounds one case; zero means 20 minutes.
	CaseTimeout time.Duration
}

// Sizes are the byte counts the bulk-transfer cases move.
type Sizes struct {
	// Tree is the total file content of the binary tar round trip.
	Tree int64
	// Pipe is what one command writes to stdout in exec-pipe-out.
	Pipe int64
	// Copy is the file CopyFile streams in.
	Copy int64
}

// PlanSizes are the plan's sizes: a 200 MB tree in and out, more than 100 MB
// piped out of one command, a 48 MB CopyFile.
func PlanSizes() Sizes {
	return Sizes{Tree: 200 << 20, Pipe: 101 << 20, Copy: 48 << 20}
}

func (target *Target) sizes() Sizes {
	if target.Sizes == (Sizes{}) {
		return PlanSizes()
	}
	return target.Sizes
}

func (target *Target) caseTimeout() time.Duration {
	if target.CaseTimeout <= 0 {
		return 20 * time.Minute
	}
	return target.CaseTimeout
}
