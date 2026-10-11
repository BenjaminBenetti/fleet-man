//go:build !nok8s

package kubernetes

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// The handle reads its workspace in the pod, never starting a session for
// it: without one a running instance's branch is not known (callers fall back
// to the last one seen), and a file comes from the host checkout the disk was
// seeded from. Before the instance has a container that checkout is the
// workspace.
func TestWorkspaceReaderWithoutASession(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("needs git")
	}
	r := newHandleRig(t)
	inst := r.inst()
	if err := os.MkdirAll(inst.WorkspaceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "seeded"}, {"-c", "user.email=a@b", "-c", "user.name=n", "commit", "-q", "--allow-empty", "-m", "x"}} {
		if out, err := exec.Command("git", append([]string{"-C", inst.WorkspaceDir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(inst.WorkspaceDir, "README.md"), []byte("seeded\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var _ backend.WorkspaceReader = (*Backend)(nil)

	// Creating: the host checkout.
	b := newHandle(inst, r.pool)
	if branch, ok := b.Branch(context.Background(), inst.WorkspaceDir); !ok || branch != "seeded" {
		t.Errorf("creating: Branch = %q %v, want the host checkout's", branch, ok)
	}

	// Running, no session: not known; the file from the seeded copy.
	inst.ContainerID = ContainerID(handleCluster, handleNS, handleSTS(), 1)
	b = newHandle(inst, r.pool)
	if branch, ok := b.Branch(context.Background(), inst.WorkspaceDir); ok {
		t.Errorf("without a session: Branch = %q, want not known", branch)
	}
	if data, err := b.ReadWorkspaceFile(context.Background(), inst.WorkspaceDir, "README.md"); err != nil || string(data) != "seeded\n" {
		t.Errorf("ReadWorkspaceFile = %q %v", data, err)
	}
	if _, err := b.ReadWorkspaceFile(context.Background(), inst.WorkspaceDir, "nope.json"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing file: %v", err)
	}
	for _, bad := range []string{"/etc/passwd", "../x", "a/../../b", ""} {
		if _, err := b.ReadWorkspaceFile(context.Background(), inst.WorkspaceDir, bad); err == nil || errors.Is(err, fs.ErrNotExist) {
			t.Errorf("path %q: %v, want refused", bad, err)
		}
	}
	if len(r.cs.Actions()) != 0 {
		t.Errorf("the reads called the API server: %v", r.cs.Actions())
	}
}
