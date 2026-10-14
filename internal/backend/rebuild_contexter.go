package backend

import "context"

// RebuildContexter is the optional capability Rebuild has as UpContexter is
// to Up: a ctx-taking Rebuild for backends whose rebuild should honour what a
// job's ctx carries (its cancellation, its provisioning log, its Hooks).
// containerID is the container being replaced, as Rebuild takes it; req is the
// workspace and mounts the new one gets.
//
// Callers go through the package-level RebuildContext, which falls back to
// Rebuild for every backend that lacks this capability.
type RebuildContexter interface {
	RebuildContext(ctx context.Context, containerID string, req UpRequest) (*UpResult, error)
}

// RebuildContext rebuilds through b's RebuildContext when b is a
// RebuildContexter, and through the plain Rebuild otherwise (which ignores
// what ctx carries).
func RebuildContext(ctx context.Context, b Backend, containerID string, req UpRequest) (*UpResult, error) {
	if rc, ok := b.(RebuildContexter); ok {
		return rc.RebuildContext(ctx, containerID, req)
	}
	return b.Rebuild(containerID, req.WorkspaceDir, req.Mounts)
}
