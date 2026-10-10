package fleet

// DeployTarget names where an instance runs. Today that is a backend type; a
// backend whose instances run somewhere it has to name (a Kubernetes cluster
// connection) adds that here, so code that picks, remembers or reports a
// target carries it without new plumbing.
//
// The zero value is "unspecified": a create request that leaves its target
// unspecified gets the daemon's remembered default.
type DeployTarget struct {
	Backend BackendType
}

// DeployTargetOption is a DeployTarget as the daemon offers it: whether a
// create can go there now and, when it cannot, why.
type DeployTargetOption struct {
	Target    DeployTarget
	Available bool
	Reason    string
}

// deployTargets lists every target this build knows, in the order the New
// instance dialog cycles them.
var deployTargets = []DeployTarget{
	{Backend: BackendDevcontainer},
	{Backend: BackendCoder},
	{Backend: BackendCodespaces},
}

// deployTargetTools maps each backend type to the CLI its instances are
// created with, on the host where fleetd runs.
var deployTargetTools = map[BackendType]string{
	BackendDevcontainer: "devcontainer",
	BackendCoder:        "coder",
	BackendCodespaces:   "gh",
}

// DeployTargets returns every target this build knows, in dialog order.
func DeployTargets() []DeployTarget {
	return append([]DeployTarget(nil), deployTargets...)
}

// IsZero reports whether the target is unspecified.
func (t DeployTarget) IsZero() bool {
	return t == DeployTarget{}
}

// String is the target's CLI form ("devcontainer").
func (t DeployTarget) String() string {
	return string(t.Backend)
}

// Tool is the CLI that creating an instance on this target runs on the daemon
// host, or "" when it needs none.
func (t DeployTarget) Tool() string {
	return deployTargetTools[t.Backend]
}

// CopiesTemplates reports whether the target can provision from a file://
// template directory copied into the workspace. Coder and Codespaces clone a
// git URL on their own side, so only devcontainer can (ValidateTemplateCreate
// enforces the same rule at create time).
func (t DeployTarget) CopiesTemplates() bool {
	return t.Backend == BackendDevcontainer
}

// Capabilities returns the target's clone/rebuild hints (see
// BackendCapabilities). The server stays authoritative: its jobs ask the live
// backend through backend.Capabilities (backendutil.CapabilitiesFor), and
// backendutil's tests keep the two tables equal.
func (t DeployTarget) Capabilities() BackendCapabilities {
	return t.Backend.Capabilities()
}

// DeployTarget returns where the instance runs. A record from before the
// Backend field (Backend "") ran on devcontainer.
func (i *Instance) DeployTarget() DeployTarget {
	if i.Backend == "" {
		return DeployTarget{Backend: BackendDevcontainer}
	}
	return DeployTarget{Backend: i.Backend}
}
