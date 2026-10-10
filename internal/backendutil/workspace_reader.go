package backendutil

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/gitutil"
)

// WorkspaceReaderFor returns the WorkspaceReader to read b's workspaces with:
// b itself when it implements backend.WorkspaceReader, else a
// HostWorkspaceReader.
func WorkspaceReaderFor(b backend.Backend) backend.WorkspaceReader {
	if wr, ok := b.(backend.WorkspaceReader); ok {
		return wr
	}
	return HostWorkspaceReader{}
}

// HostWorkspaceReader reads an instance's workspaceDir on the daemon host:
// the checkout a devcontainer instance bind-mounts, and the clone codespaces
// and coder instances keep from create. It answers whether or not the instance
// is running, so a stopped instance still shows the branch it is on.
type HostWorkspaceReader struct{}

// Branch reads HEAD of the host checkout. It reports ok=false when there is
// no checkout to read (no workspaceDir, or the directory is gone) or ctx ended
// before git answered, so the caller falls back rather than showing a blank.
func (HostWorkspaceReader) Branch(ctx context.Context, workspaceDir string) (string, bool) {
	if workspaceDir == "" {
		return "", false
	}
	if _, err := os.Stat(workspaceDir); err != nil {
		return "", false
	}
	branch := gitutil.BranchName(ctx, workspaceDir)
	if ctx.Err() != nil {
		return "", false
	}
	return branch, true
}

// ReadWorkspaceFile reads relPath under the host checkout.
func (HostWorkspaceReader) ReadWorkspaceFile(_ context.Context, workspaceDir, relPath string) ([]byte, error) {
	if !fs.ValidPath(relPath) {
		return nil, fmt.Errorf("invalid workspace path %q", relPath)
	}
	if workspaceDir == "" {
		return nil, fmt.Errorf("read %s: no workspace: %w", relPath, fs.ErrNotExist)
	}
	return os.ReadFile(filepath.Join(workspaceDir, filepath.FromSlash(relPath)))
}
