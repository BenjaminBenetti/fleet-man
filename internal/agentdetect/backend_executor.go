package agentdetect

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// ===========================================
// BackendExecutor
// ===========================================

// BackendExecutor adapts a backend.Backend's ExecCommand into the
// ContainerExecutor surface the provisioner consumes.
//
// The provisioner is deliberately decoupled from the backend
// package — it knows nothing about devcontainer vs Coder vs
// Codespaces — so this adapter is the single place that concrete
// transport details (which exec wrapper, which workspace dir) get
// wired in.
type BackendExecutor struct {
	backend      backend.Backend
	workspaceDir string
}

// ===========================================
// Constructors
// ===========================================

// NewBackendExecutor returns a ContainerExecutor that runs commands
// against the given backend's ExecCommand surface, anchored at the
// given workspace directory (the same wsDir create.Run uses for
// dotfiles install and other setup steps).
func NewBackendExecutor(target backend.Backend, workspaceDir string) *BackendExecutor {
	return &BackendExecutor{backend: target, workspaceDir: workspaceDir}
}

// ===========================================
// ContainerExecutor implementation
// ===========================================

// Run delegates to backend.ExecCommand with stdin / stdout / stderr
// wired into in-memory buffers. On non-zero exit the wrapped error
// includes the trimmed stderr so callers have a useful diagnostic
// without having to manage pipes themselves.
func (b *BackendExecutor) Run(args []string, stdin []byte) ([]byte, error) {
	cmd := b.backend.ExecCommand(b.workspaceDir, args)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return nil, fmt.Errorf("%w: %s", err, detail)
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

// CopyFile delegates to the backend's stdin-EOF-safe file transfer, anchored at
// the same workspace dir. It is the uncapped counterpart to an inline write,
// used for payloads (the merged settings.json) that may exceed the inline cap.
func (b *BackendExecutor) CopyFile(src io.Reader, remotePath string, mode int) error {
	return b.backend.CopyFile(b.workspaceDir, src, remotePath, mode)
}
