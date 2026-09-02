package create

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCopyTemplateTreeCopiesContentsVerbatim covers the cp-for-clone swap:
// the template's files, nested dirs, and any .git land inside wsDir, and the
// template itself is untouched afterwards.
func TestCopyTemplateTreeCopiesContentsVerbatim(t *testing.T) {
	tmpl := t.TempDir()
	for rel, body := range map[string]string{
		"README.md":                       "hello",
		".devcontainer/devcontainer.json": "{}",
		".git/HEAD":                       "ref: refs/heads/main",
		"src/nested/deep.txt":             "deep",
	} {
		full := filepath.Join(tmpl, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}

	wsDir := filepath.Join(t.TempDir(), "fleet", "inst", "fleet")
	if err := copyTemplateTree("file://"+tmpl, wsDir); err != nil {
		t.Fatalf("copyTemplateTree: %v", err)
	}
	for rel, want := range map[string]string{
		"README.md":                       "hello",
		".devcontainer/devcontainer.json": "{}",
		".git/HEAD":                       "ref: refs/heads/main",
		"src/nested/deep.txt":             "deep",
	} {
		got, err := os.ReadFile(filepath.Join(wsDir, rel))
		if err != nil {
			t.Fatalf("%s not copied: %v", rel, err)
		}
		if string(got) != want {
			t.Fatalf("%s = %q, want %q", rel, got, want)
		}
	}

	// Editing the copy must not reach back into the template.
	if err := os.WriteFile(filepath.Join(wsDir, "README.md"), []byte("edited"), 0644); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(tmpl, "README.md")); string(got) != "hello" {
		t.Fatalf("template README changed to %q", got)
	}
}

// Like git clone, a non-empty destination is refused rather than merged under
// the template — a stale workspace dir must surface, not silently mix in.
func TestCopyTemplateTreeRefusesNonEmptyDestination(t *testing.T) {
	tmpl := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpl, "a.txt"), []byte("a"), 0644); err != nil {
		t.Fatal(err)
	}
	wsDir := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(wsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, "stale.txt"), []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	err := copyTemplateTree("file://"+tmpl, wsDir)
	if err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("want non-empty destination error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(wsDir, "a.txt")); err == nil {
		t.Fatal("template must not have been copied over a non-empty dir")
	}

	// An existing but EMPTY dir is fine (MkdirAll of the parent is common).
	empty := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(empty, 0755); err != nil {
		t.Fatal(err)
	}
	if err := copyTemplateTree("file://"+tmpl, empty); err != nil {
		t.Fatalf("empty destination should be accepted: %v", err)
	}
}

func TestTemplateGitFileWarning(t *testing.T) {
	ws := t.TempDir()
	if got := templateGitFileWarning(ws); got != "" {
		t.Fatalf("no .git: want no warning, got %q", got)
	}
	if err := os.MkdirAll(filepath.Join(ws, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if got := templateGitFileWarning(ws); got != "" {
		t.Fatalf(".git dir: want no warning, got %q", got)
	}
	ws2 := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws2, ".git"), []byte("gitdir: /elsewhere/.git/worktrees/x"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := templateGitFileWarning(ws2); !strings.Contains(got, "worktree") {
		t.Fatalf(".git file: want worktree warning, got %q", got)
	}
}

func TestCopyTemplateTreeRejectsBadSources(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "ws")
	if err := copyTemplateTree("file://"+filepath.Join(t.TempDir(), "missing"), dest); err == nil {
		t.Fatal("missing template dir should error")
	}
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := copyTemplateTree("file://"+file, dest); err == nil {
		t.Fatal("template path that is a file should error")
	}
	if err := copyTemplateTree("git@github.com:o/r.git", dest); err == nil {
		t.Fatal("non-template remote should error")
	}
}
