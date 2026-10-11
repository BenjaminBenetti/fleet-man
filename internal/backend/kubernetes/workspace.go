//go:build !nok8s

package kubernetes

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/gitutil"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
	"github.com/BenjaminBenetti/fleet-man/internal/shellquote"
)

// workspace.go is the handle's backend.WorkspaceReader: an instance's
// workspace lives on its disk in the pod, where the user switches branches
// and edits files, so it is read there, over the pod's live agent session
// (as the remote user, whose checkout it is). Like the polls it never starts
// a session: a stopped or reconnecting instance's branch is not known
// (callers fall back to Instance.last_branch). Before the instance has a
// container, the host checkout its disk is seeded from is the workspace.

// exitNoFile is what the file read exits with for a missing file.
const exitNoFile = 44

// Branch reads the branch checked out in the instance's workspace folder:
// ("", true) when HEAD is detached or the folder is not a git checkout, ok
// false when there is no live session to read it over.
func (b *Backend) Branch(ctx context.Context, workspaceDir string) (string, bool) {
	if b.inst.ContainerID == "" {
		return hostBranch(ctx, workspaceDir)
	}
	folder, err := b.workspaceFolder()
	if err != nil {
		return "", false
	}
	res, ok := b.pollExec(ctx, "cd "+shellquote.Single(folder)+" 2>/dev/null || exit 3\n"+
		"b=$(git symbolic-ref --short -q HEAD 2>/dev/null) || b=\nprintf '%s' \"$b\"")
	if !ok || res.Code != 0 {
		return "", false
	}
	return strings.TrimSpace(string(res.Stdout)), true
}

// ReadWorkspaceFile reads relPath under the instance's workspace folder in
// the pod; without a live session (or before the instance has a container)
// it reads the host checkout the disk was seeded from.
func (b *Backend) ReadWorkspaceFile(ctx context.Context, workspaceDir, relPath string) ([]byte, error) {
	if !fs.ValidPath(relPath) {
		return nil, fmt.Errorf("invalid workspace path %q", relPath)
	}
	if b.inst.ContainerID != "" {
		if folder, err := b.workspaceFolder(); err == nil {
			file := shellquote.Single(path.Join(folder, relPath))
			res, ok := b.pollExec(ctx, "[ -f "+file+" ] || exit "+fmt.Sprint(exitNoFile)+"\ncat "+file)
			switch {
			case ok && res.Code == 0:
				return res.Stdout, nil
			case ok && res.Code == exitNoFile:
				return nil, fmt.Errorf("read %s in the instance: %w", relPath, fs.ErrNotExist)
			case ok:
				return nil, fmt.Errorf("read %s in the instance: exit status %d: %s", relPath, res.Code, strings.TrimSpace(string(res.Stderr)))
			}
		}
	}
	if workspaceDir == "" {
		return nil, fmt.Errorf("read %s: no workspace: %w", relPath, fs.ErrNotExist)
	}
	return os.ReadFile(filepath.Join(workspaceDir, filepath.FromSlash(relPath)))
}

// workspaceFolder is the devcontainer's workspace folder in the pod, from
// the instance record.
func (b *Backend) workspaceFolder() (string, error) {
	id, err := b.identity()
	if err != nil {
		return "", err
	}
	rec, err := LoadInstanceRecord(id.cluster, id.key)
	if err != nil {
		return "", err
	}
	if rec.WorkspaceFolder == "" {
		return "", fmt.Errorf("the instance record names no workspace folder")
	}
	return rec.WorkspaceFolder, nil
}

// pollExec runs a script as the remote user (the exec class) on the
// instance's live session, bounded by pollTimeout; false without a session
// or when it could not run.
func (b *Backend) pollExec(ctx context.Context, text string) (*podwire.RunResult, bool) {
	s := b.liveSession(b.inst.ContainerID)
	if s == nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()
	res, err := s.Client().Run(ctx, podwire.ExecHeader{Request: execas.Request{Class: execas.ClassExec, Argv: []string{"sh", "-c", text}}}, nil)
	if err != nil {
		return nil, false
	}
	return res, true
}

// hostBranch reads HEAD of the host checkout, as backendutil's
// HostWorkspaceReader does.
func hostBranch(ctx context.Context, workspaceDir string) (string, bool) {
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
