package gitutil

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBranchNameReturnsBranchForGitRepo(t *testing.T) {
	requireGit(t)

	repo := initCommittedRepo(t)
	runGit(t, repo, "checkout", "-b", "feature/status-line")

	if got := BranchName(context.Background(), repo); got != "feature/status-line" {
		t.Fatalf("BranchName() = %q, want %q", got, "feature/status-line")
	}
}

func TestBranchNameReturnsEmptyForNonRepoDirectory(t *testing.T) {
	if got := BranchName(context.Background(), t.TempDir()); got != "" {
		t.Fatalf("BranchName() = %q, want empty string", got)
	}
}

func TestBranchNameReturnsEmptyForDetachedHEAD(t *testing.T) {
	requireGit(t)

	repo := initCommittedRepo(t)
	runGit(t, repo, "checkout", "--detach")

	if got := BranchName(context.Background(), repo); got != "" {
		t.Fatalf("BranchName() = %q, want empty string", got)
	}
}

func TestBranchNameReturnsEmptyForMissingWorkspace(t *testing.T) {
	missingPath := filepath.Join(t.TempDir(), "missing")
	if got := BranchName(context.Background(), missingPath); got != "" {
		t.Fatalf("BranchName() = %q, want empty string", got)
	}
}

func requireGit(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

func initCommittedRepo(t *testing.T) string {
	t.Helper()

	repo := t.TempDir()
	runGit(t, repo, "init")

	readmePath := filepath.Join(repo, "README.md")
	if err := os.WriteFile(readmePath, []byte("fleet\n"), 0644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "-c", "user.name=Fleet Tests", "-c", "user.email=fleet-tests@example.com", "commit", "-m", "init")

	return repo
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}
