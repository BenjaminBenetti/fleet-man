package agentdetect

import "io"

// ===========================================
// ContainerExecutor abstraction
// ===========================================

// ContainerExecutor is the minimal "run a command inside the target
// container" surface the provisioner depends on. Backends supply an
// implementation via NewBackendExecutor; tests supply a stub.
//
// Run executes args (already in argv form — typically
// []{"sh", "-c", "..."}) and returns the stdout bytes. When stdin
// is non-nil it is fed to the command's standard input. Any non-zero
// exit or transport error is returned as err with stderr included
// in the wrapped message so the caller has something diagnostic to
// surface to the user.
//
// CopyFile writes src to remotePath with the given mode using the
// backend's stdin-EOF-safe file transfer (atomic, parent-creating).
// Unlike an inline base64-in-argv write it carries no size cap, so the
// provisioner uses it for the unbounded read-merge-rewrite of the
// user's settings.json; small fixed payloads still go inline.
type ContainerExecutor interface {
	Run(args []string, stdin []byte) ([]byte, error)
	CopyFile(src io.Reader, remotePath string, mode int) error
}
