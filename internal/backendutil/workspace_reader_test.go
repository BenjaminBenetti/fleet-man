package backendutil

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// readerBackend is a Backend that implements its own WorkspaceReader, the
// shape a backend whose workspace lives in the instance (kubernetes) takes.
type readerBackend struct {
	backend.Backend
}

func (readerBackend) Branch(context.Context, string) (string, bool) { return "from-instance", true }

func (readerBackend) ReadWorkspaceFile(context.Context, string, string) ([]byte, error) {
	return []byte("from-instance"), nil
}

func TestWorkspaceReaderForPrefersTheBackendsOwnReader(t *testing.T) {
	if _, ok := WorkspaceReaderFor(readerBackend{}).(readerBackend); !ok {
		t.Fatal("a backend implementing WorkspaceReader must be used as its own reader")
	}
	for _, bt := range []fleet.BackendType{fleet.BackendDevcontainer, fleet.BackendCodespaces, fleet.BackendCoder} {
		if _, ok := WorkspaceReaderFor(New(bt, false)).(HostWorkspaceReader); !ok {
			t.Fatalf("%s: want the host reader (the checkout lives on the daemon host)", bt)
		}
	}
}

func TestHostWorkspaceReaderBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("commit", "-q", "--allow-empty", "-m", "init")
	git("checkout", "-q", "-b", "feature/x")

	r := HostWorkspaceReader{}
	ctx := context.Background()
	if got, ok := r.Branch(ctx, repo); !ok || got != "feature/x" {
		t.Fatalf("Branch(repo) = %q, %v; want feature/x, true", got, ok)
	}

	git("checkout", "-q", "--detach")
	if got, ok := r.Branch(ctx, repo); !ok || got != "" {
		t.Fatalf("Branch(detached) = %q, %v; want \"\", true (read, but no named branch)", got, ok)
	}
	if got, ok := r.Branch(ctx, t.TempDir()); !ok || got != "" {
		t.Fatalf("Branch(not a repo) = %q, %v; want \"\", true", got, ok)
	}

	if _, ok := r.Branch(ctx, ""); ok {
		t.Fatal("Branch(\"\") must report no read")
	}
	if _, ok := r.Branch(ctx, filepath.Join(repo, "missing")); ok {
		t.Fatal("Branch(missing dir) must report no read")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, ok := r.Branch(cancelled, repo); ok {
		t.Fatal("Branch with an ended ctx must report no read, not a blank branch")
	}
}

func TestHostWorkspaceReaderReadWorkspaceFile(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".devcontainer"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".devcontainer", "devcontainer.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := HostWorkspaceReader{}
	ctx := context.Background()
	got, err := r.ReadWorkspaceFile(ctx, ws, ".devcontainer/devcontainer.json")
	if err != nil || string(got) != "{}" {
		t.Fatalf("ReadWorkspaceFile = %q, %v; want {}", got, err)
	}
	if _, err := r.ReadWorkspaceFile(ctx, ws, ".devcontainer.json"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing file: err = %v, want fs.ErrNotExist", err)
	}
	if _, err := r.ReadWorkspaceFile(ctx, "", ".devcontainer.json"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("no workspace: err = %v, want fs.ErrNotExist (not a read relative to the daemon's cwd)", err)
	}
	for _, bad := range []string{"/etc/passwd", "../outside", ".devcontainer/../../x", ""} {
		if _, err := r.ReadWorkspaceFile(ctx, ws, bad); err == nil || errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("ReadWorkspaceFile(%q): err = %v, want an invalid-path error", bad, err)
		}
	}
}
