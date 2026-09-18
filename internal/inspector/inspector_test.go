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

// TestOpenTemplateUsesDirInPlace verifies the file:// path: no clone, Root
// IS the template directory, and Close leaves the user's directory alone.
func TestOpenTemplateUsesDirInPlace(t *testing.T) {
	dir := t.TempDir()
	dcDir := filepath.Join(dir, ".devcontainer")
	if err := os.MkdirAll(dcDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dcDir, "devcontainer.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	repo, err := Open("file://"+dir, "ignored-branch")
	if err != nil {
		t.Fatalf("Open(file://): %v", err)
	}
	if repo.Root != dir {
		t.Fatalf("Root = %q, want the template dir %q", repo.Root, dir)
	}
	if _, _, err := repo.FindDevcontainerJSON(); err != nil {
		t.Fatalf("FindDevcontainerJSON on template: %v", err)
	}
	if err := repo.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(dcDir); err != nil {
		t.Fatalf("Close must not delete the template dir: %v", err)
	}
}

func TestOpenTemplateRejectsMissingOrFile(t *testing.T) {
	if _, err := Open("file://"+filepath.Join(t.TempDir(), "nope"), ""); err == nil {
		t.Fatal("missing template dir should error")
	}
	file := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(file, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open("file://"+file, ""); err == nil {
		t.Fatal("template path that is a file should error")
	}
	if _, err := Open("file://relative/path", ""); err == nil {
		t.Fatal("relative template path should error")
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
