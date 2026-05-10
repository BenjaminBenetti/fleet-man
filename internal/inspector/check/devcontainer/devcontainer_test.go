package devcontainer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/inspector"
)

// TestPresentReturnsTrueForCanonicalLocation verifies the common case:
// a .devcontainer/devcontainer.json sitting at the repo root.
func TestPresentReturnsTrueForCanonicalLocation(t *testing.T) {
	repo := makeRepo(t)
	dcDir := filepath.Join(repo.Root, ".devcontainer")
	if err := os.MkdirAll(dcDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dcDir, "devcontainer.json"), []byte("{}"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	present, err := Present(repo)
	if err != nil {
		t.Fatalf("Present: %v", err)
	}
	if !present {
		t.Fatalf("Present = false, want true")
	}
}

// TestPresentReturnsFalseWhenMissing makes sure an absent devcontainer
// config surfaces as (false, nil) rather than an error — callers branch
// on the bool to decide whether to prompt the user.
func TestPresentReturnsFalseWhenMissing(t *testing.T) {
	repo := makeRepo(t)
	present, err := Present(repo)
	if err != nil {
		t.Fatalf("Present: %v", err)
	}
	if present {
		t.Fatalf("Present = true, want false")
	}
}

// makeRepo creates a temporary directory and returns it wrapped in an
// *inspector.Repo so tests can drive Present without going through the
// real shallow-clone path.
func makeRepo(t *testing.T) *inspector.Repo {
	t.Helper()
	dir := t.TempDir()
	return &inspector.Repo{Root: dir}
}
