package inspector

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestFindDevcontainerJSONPrefersStandardLocation verifies the search
// order: the canonical .devcontainer/devcontainer.json wins even when
// an alternative also exists at the repo root.
func TestFindDevcontainerJSONPrefersStandardLocation(t *testing.T) {
	repo := newRepoWithFiles(t, map[string]string{
		".devcontainer/devcontainer.json": `{"remoteUser":"a"}`,
		".devcontainer.json":              `{"remoteUser":"b"}`,
	})

	path, contents, err := repo.FindDevcontainerJSON()
	if err != nil {
		t.Fatalf("FindDevcontainerJSON() error = %v", err)
	}
	want := filepath.Join(repo.Root, ".devcontainer", "devcontainer.json")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if string(contents) != `{"remoteUser":"a"}` {
		t.Errorf("wrong file picked: %q", contents)
	}
}

// TestFindDevcontainerJSONFallsBackToRootFile verifies that when the
// canonical location is missing the repo-root variant is found.
func TestFindDevcontainerJSONFallsBackToRootFile(t *testing.T) {
	repo := newRepoWithFiles(t, map[string]string{
		".devcontainer.json": `{"remoteUser":"vscode"}`,
	})

	path, _, err := repo.FindDevcontainerJSON()
	if err != nil {
		t.Fatalf("FindDevcontainerJSON() error = %v", err)
	}
	want := filepath.Join(repo.Root, ".devcontainer.json")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}

// TestFindDevcontainerJSONFindsSubfolderLayout covers the
// .devcontainer/<name>/devcontainer.json layout used for
// multi-container setups.
func TestFindDevcontainerJSONFindsSubfolderLayout(t *testing.T) {
	repo := newRepoWithFiles(t, map[string]string{
		".devcontainer/frontend/devcontainer.json": `{"remoteUser":"node"}`,
	})

	path, _, err := repo.FindDevcontainerJSON()
	if err != nil {
		t.Fatalf("FindDevcontainerJSON() error = %v", err)
	}
	want := filepath.Join(repo.Root, ".devcontainer", "frontend", "devcontainer.json")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}

// TestFindDevcontainerJSONErrorsWhenAbsent surfaces a sentinel error
// when no config is present so callers can distinguish "not a
// devcontainer repo" from other failures.
func TestFindDevcontainerJSONErrorsWhenAbsent(t *testing.T) {
	repo := newRepoWithFiles(t, nil)

	_, _, err := repo.FindDevcontainerJSON()
	if !errors.Is(err, ErrNoDevcontainerConfig) {
		t.Fatalf("err = %v, want ErrNoDevcontainerConfig", err)
	}
}

// TestCloseRemovesRootDir verifies the lifecycle contract: after Close,
// the temp clone is gone.
func TestCloseRemovesRootDir(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, "clone")
	if err := os.Mkdir(subdir, 0755); err != nil {
		t.Fatal(err)
	}
	repo := &Repo{Root: subdir}
	if err := repo.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := os.Stat(subdir); !os.IsNotExist(err) {
		t.Fatalf("expected %s removed, stat err = %v", subdir, err)
	}
}

// TestCloseOnNilOrZeroValueIsSafe documents that Close is a no-op for
// uninitialised receivers — the production code defers Close on the
// success path of Open, but the helper should not panic if invoked on
// a Repo whose Root field is empty.
func TestCloseOnNilOrZeroValueIsSafe(t *testing.T) {
	var nilRepo *Repo
	if err := nilRepo.Close(); err != nil {
		t.Errorf("nil Close() error = %v", err)
	}
	zero := &Repo{}
	if err := zero.Close(); err != nil {
		t.Errorf("zero Close() error = %v", err)
	}
}

// newRepoWithFiles writes the given path→contents map under a fresh
// temp dir and returns a Repo rooted there. Used in lieu of a real
// git clone for unit tests of repo-shaped logic.
func newRepoWithFiles(t *testing.T, files map[string]string) *Repo {
	t.Helper()
	root := t.TempDir()
	for relPath, contents := range files {
		full := filepath.Join(root, relPath)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(contents), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return &Repo{Root: root}
}
