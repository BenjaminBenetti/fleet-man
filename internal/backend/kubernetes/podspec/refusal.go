package podspec

import (
	"errors"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
)

// Error codes a translation fails with, besides the per-option
// kubernetes:unsupported:flag:<option> (kubeshim.UnsupportedFlag). Each
// error message starts with its code.
const (
	// CodeHostSpecialFile is a bind of a device or FIFO on the daemon host,
	// which cannot reach a pod.
	CodeHostSpecialFile = "kubernetes:unsupported:host-special-file"
	// CodeHostMount is a fleet mount of a daemon-host path, which a pod
	// cannot bind.
	CodeHostMount = "kubernetes:unsupported:host-mount"
	// CodePrivileged is a run that needs a privileged pod on a connection
	// that does not allow them.
	CodePrivileged = "kubernetes:unsupported:privileged"
	// CodeDooDNeedsPrivileged is a run that reaches the docker engine (a
	// docker.sock bind) on a connection that allows neither privileged
	// pods nor a sysbox runtime class: the engine is the pod's own sidecar,
	// which needs one of them.
	CodeDooDNeedsPrivileged = "kubernetes:unsupported:dood-needs-privileged"
	// CodeDooDNeedsSidecars is a run that reaches the docker engine on a
	// cluster too old for native sidecars (Kubernetes 1.29), which the
	// pod's engine runs as.
	CodeDooDNeedsSidecars = "kubernetes:unsupported:dood-needs-sidecars"
	// CodeNoCommand is a run with nothing to run.
	CodeNoCommand = "kubernetes:unsupported:no-command"
	// CodeSpecTooLarge is a spec that does not fit in a Secret.
	CodeSpecTooLarge = "kubernetes:unsupported:spec-size"
	// CodeSettings is a connection setting (or a devcontainer value) that
	// does not read; the daemon validates settings when they are saved.
	CodeSettings = "kubernetes:misconfigured"
	// CodeAdmissionDenied starts the error of a render admission refused.
	CodeAdmissionDenied = "kubernetes:admission_denied"
	// CodeUnsupportedMount is a mount fleet cannot provide in a pod yet (a
	// host socket or directory, the fleet's share).
	CodeUnsupportedMount = "kubernetes:unsupported:mount"
	// CodeQuota is a ResourceQuota that has no room for the instance.
	CodeQuota = "kubernetes:quota"
	// CodeForbidden is a permission fleet's credential lacks.
	CodeForbidden = "kubernetes:forbidden"
	// CodeInvalidSpec is a spec the API server finds invalid: something
	// the run asked for that this cluster's API does not take.
	CodeInvalidSpec = "kubernetes:unsupported:spec"
)

// Refusal is one reason a run cannot become a pod.
type Refusal struct {
	// Code is the error code (kubernetes:unsupported:flag:privileged).
	Code string
	// Reason says what was asked and what to do instead.
	Reason string
}

func (r *Refusal) Error() string {
	return r.Code + ": " + r.Reason
}

// refuseOption is a Refusal of the option name (canonical, no dashes).
func refuseOption(name, reason string) *Refusal {
	return &Refusal{Code: kubeshim.UnsupportedFlag(name), Reason: reason}
}

// Refusals returns every Refusal in err, which Translate joins when a run
// has several.
func Refusals(err error) []*Refusal {
	var out []*Refusal
	var walk func(error)
	walk = func(err error) {
		if r, ok := err.(*Refusal); ok {
			out = append(out, r)
			return
		}
		// A classified denial (DryRun's) is a refusal too.
		if r, ok := err.(interface{ Refusal() *Refusal }); ok {
			out = append(out, r.Refusal())
			return
		}
		if j, ok := err.(interface{ Unwrap() []error }); ok {
			for _, e := range j.Unwrap() {
				walk(e)
			}
			return
		}
		if e := errors.Unwrap(err); e != nil {
			walk(e)
		}
	}
	if err != nil {
		walk(err)
	}
	return out
}

// joinRefusals is the error of a translation that collected refusals: the
// first one's code leads, so the create's error is classified by it, and
// every one is listed.
func joinRefusals(rs []*Refusal) error {
	switch len(rs) {
	case 0:
		return nil
	case 1:
		return rs[0]
	}
	return &refusals{rs: rs}
}

type refusals struct{ rs []*Refusal }

func (e *refusals) Error() string {
	parts := make([]string, len(e.rs))
	for i, r := range e.rs {
		parts[i] = r.Error()
	}
	return strings.Join(parts, "; ")
}

func (e *refusals) Unwrap() []error {
	out := make([]error, len(e.rs))
	for i, r := range e.rs {
		out[i] = r
	}
	return out
}
