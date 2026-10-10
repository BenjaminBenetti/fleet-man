package fleet

// DeployTarget names where an instance runs: a backend type and, for
// Kubernetes, the cluster connection its instances run on. Code that picks,
// remembers or reports a target carries both.
//
// The zero value is "unspecified": a create request that leaves its target
// unspecified gets the daemon's remembered default.
type DeployTarget struct {
	Backend BackendType
	// KubeCluster is the cluster connection (KubeCluster.ID) of a Kubernetes
	// target; empty for every other backend.
	KubeCluster string
}

// DeployTargetOption is a DeployTarget as the daemon offers it: whether a
// create can go there now and, when it cannot, why.
type DeployTargetOption struct {
	Target    DeployTarget
	Available bool
	Reason    string
}

// deployTargets lists every target this build knows that needs no
// connection, in the order the New instance dialog cycles them. Kubernetes has
// one target per cluster connection, which only the daemon knows, so it lists
// them itself (after these).
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
	// A cluster's image is built on the daemon host by the devcontainer CLI
	// (which also needs docker there).
	BackendKubernetes: "devcontainer",
}

// DeployTargets returns every target this build knows that needs no
// connection, in dialog order (see deployTargets).
func DeployTargets() []DeployTarget {
	return append([]DeployTarget(nil), deployTargets...)
}

// IsZero reports whether the target is unspecified.
func (t DeployTarget) IsZero() bool {
	return t == DeployTarget{}
}

// String is the target's CLI form: the backend ("devcontainer"), and for a
// cluster target the connection after a colon ("kubernetes:minikube").
func (t DeployTarget) String() string {
	if t.KubeCluster != "" {
		return string(t.Backend) + ":" + t.KubeCluster
	}
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
	return DeployTarget{Backend: i.Backend, KubeCluster: i.KubeCluster}
}
