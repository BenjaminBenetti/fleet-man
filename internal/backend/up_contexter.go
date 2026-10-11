package backend

import "context"

// UpRequest is what an UpContext call provisions: Backend.Up's arguments as a
// struct, so the request can grow without another interface change.
type UpRequest struct {
	// WorkspaceDir is the host workspace folder, exactly as Up takes it.
	WorkspaceDir string
	// Mounts are the caller-supplied bind mounts, exactly as Up takes them.
	Mounts []Mount
	// Layout is the fleet-shared mounts' tree as the resolver made it on the
	// daemon host (resolver.Resolved.Layout). A backend that binds the host
	// paths ignores it; one that keeps the fleet's shared state elsewhere (a
	// cluster's share volume) makes the same tree there.
	Layout []LayoutEntry
}

// UpContexter is an optional capability for backends whose provisioning is a
// long, multi-step sequence that has to stop when its job is cancelled
// (Kubernetes: build, seed, apply, wait for the pod). For those backends
// UpContext replaces Up. ctx carries the job's cancellation, its provisioning
// log (JobLogFrom, with ReportStep / ReportWarning) and the Hooks the server
// injected (HooksFrom). An implementation uses what applies to it: the
// devcontainer backend only tees its CLI's output into the log.
//
// Callers go through the package-level UpContext, which falls back to Up for
// every backend that lacks this capability.
type UpContexter interface {
	UpContext(ctx context.Context, req UpRequest) (*UpResult, error)
}

// UpContext provisions req through b's UpContext when b is an UpContexter, and
// through the plain Up otherwise. Up takes no ctx, so for those backends the
// cancellation, JobLog and Hooks that ctx carries are ignored.
func UpContext(ctx context.Context, b Backend, req UpRequest) (*UpResult, error) {
	if uc, ok := b.(UpContexter); ok {
		return uc.UpContext(ctx, req)
	}
	return b.Up(req.WorkspaceDir, req.Mounts)
}
