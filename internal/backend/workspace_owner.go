package backend

// WorkspaceOwner is an optional capability for backends that keep an
// instance's workspace (its checkout) in their own runtime, not in the host
// workspaceDir that devcontainer bind-mounts and that codespaces and coder keep
// from their create: Kubernetes keeps it on the instance's volume. For those,
// the host directory is at most the staging copy a create seeds the instance
// from, so work on the workspace goes through the backend: a clone copies it
// with the rest of the instance instead of copying the host directory, and
// reads go through WorkspaceReader.
type WorkspaceOwner interface {
	// OwnsWorkspace reports whether the bound instance's workspace lives in
	// the backend's runtime rather than in its host workspaceDir.
	OwnsWorkspace() bool
}

// OwnsWorkspace reports whether b keeps its instance's workspace itself
// (WorkspaceOwner). Every other backend works on the host workspaceDir.
func OwnsWorkspace(b Capabilities) bool {
	owner, ok := b.(WorkspaceOwner)
	return ok && owner.OwnsWorkspace()
}
