package create

import (
	"os"
	"path/filepath"
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
