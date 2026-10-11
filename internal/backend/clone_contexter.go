package backend

import "context"

// CloneContexter is the optional capability Clone has as UpContexter is to
// Up: a ctx-taking Clone for backends whose clone is a long, multi-step
// sequence that must honour what a job's ctx carries (its cancellation, its
// provisioning log, its Hooks). sourceContainerID is the instance cloned, as
// Clone takes it; req is the workspace and mounts the clone gets.
//
// Callers go through the package-level CloneContext, which falls back to
// Clone for every backend that lacks this capability.
type CloneContexter interface {
	CloneContext(ctx context.Context, sourceContainerID string, req UpRequest) (*UpResult, error)
}

// CloneContext clones through b's CloneContext when b is a CloneContexter,
// and through the plain Clone otherwise (which ignores what ctx carries).
func CloneContext(ctx context.Context, b Backend, sourceContainerID string, req UpRequest) (*UpResult, error) {
	if cc, ok := b.(CloneContexter); ok {
		return cc.CloneContext(ctx, sourceContainerID, req)
	}
	return b.Clone(sourceContainerID, req.WorkspaceDir, req.Mounts)
}
