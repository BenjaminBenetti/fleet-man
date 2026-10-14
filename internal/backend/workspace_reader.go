package backend

import "context"

// WorkspaceReader is an optional capability for backends whose workspace — the
// checkout an instance works on — is not the host directory recorded as the
// instance's workspaceDir, so reading it takes a trip into the instance (a pod
// volume, say). Server code reaches it through backendutil.WorkspaceReaderFor,
// which falls back to reading workspaceDir on the daemon host for backends that
// don't implement it (devcontainer bind-mounts that directory, and codespaces
// and coder keep the host clone made at create), so it works whether or not the
// instance is running.
//
// Every read is daemon-side: clients never read a checkout themselves, because a
// client may be on another machine than the daemon (the gateway, Armada SSH).
type WorkspaceReader interface {
	// Branch returns the branch checked out in the workspace. ok is false when
	// the workspace can't be read right now (for example its only copy lives in
	// a runtime that is stopped); callers then fall back to the instance's
	// LastBranch. ("", true) means the read worked but HEAD is not on a named
	// branch: detached, or the workspace is not a git repository.
	Branch(ctx context.Context, workspaceDir string) (branch string, ok bool)

	// ReadWorkspaceFile returns the contents of relPath, a slash-separated path
	// relative to the workspace root (fs.ValidPath rules: no leading slash, no
	// "..", no "." elements). A file that doesn't exist is reported with an
	// error satisfying errors.Is(err, fs.ErrNotExist).
	ReadWorkspaceFile(ctx context.Context, workspaceDir, relPath string) ([]byte, error)
}
