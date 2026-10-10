package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestImportGraph(t *testing.T) {
	out := []byte(`k8s.io/api/core
k8s.io/client-go/kubernetes k8s.io/api/core
example.com/m/internal/kube k8s.io/client-go/kubernetes
example.com/m/internal/other
example.com/m/internal/server example.com/m/internal/other example.com/m/internal/kube
example.com/m/cmd/fleet example.com/m/internal/server example.com/m/internal/other
`)
	g, err := parseImportGraph(out)
	if err != nil {
		t.Fatal(err)
	}
	if g.root() != "example.com/m/cmd/fleet" {
		t.Errorf("root = %q", g.root())
	}
	// Dependency order: the deepest k8s.io package comes first.
	if got := g.firstMatch(k8sModules); got != "k8s.io/api/core" {
		t.Errorf("firstMatch = %q", got)
	}
	if got := g.firstMatch([]string{"sigs.k8s.io/"}); got != "" {
		t.Errorf("firstMatch(sigs.k8s.io/) = %q, want none", got)
	}
	want := []string{
		"example.com/m/cmd/fleet",
		"example.com/m/internal/server",
		"example.com/m/internal/kube",
		"k8s.io/client-go/kubernetes",
		"k8s.io/api/core",
	}
	if got := g.chain("k8s.io/api/core"); !reflect.DeepEqual(got, want) {
		t.Errorf("chain = %v, want %v", got, want)
	}
	if got := g.chain("not/in/graph"); !reflect.DeepEqual(got, []string{"example.com/m/cmd/fleet", "...", "not/in/graph"}) {
		t.Errorf("chain(unreachable) = %v", got)
	}
}

func TestParseImportGraphEmpty(t *testing.T) {
	if _, err := parseImportGraph([]byte("\n")); err == nil {
		t.Error("want an error for empty go list output")
	}
}

// TestCheckNoK8sModule runs the real check over a throwaway module whose
// k8s.io dependency is a local replace, so it needs no network: the import is
// caught when unguarded and dropped when its file carries //go:build !nok8s.
func TestCheckNoK8sModule(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go list")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/m\n\ngo 1.21\n\nrequire k8s.io/fake v0.0.0\n\nreplace k8s.io/fake => ./fake\n")
	write("fake/go.mod", "module k8s.io/fake\n\ngo 1.21\n")
	write("fake/fake.go", "package fake\n\nconst Name = \"fake\"\n")
	write("kube/kube.go", "package kube\n\nimport \"k8s.io/fake\"\n\nvar Name = fake.Name\n")
	write("main.go", "package main\n\nfunc main() {}\n")
	write("wire.go", "package main\n\nimport _ \"example.com/m/kube\"\n")
	t.Setenv("GOFLAGS", "-mod=mod")
	t.Setenv("GOWORK", "off")
	t.Setenv("GOPROXY", "off")

	err := checkNoK8s(dir, ".", []string{"linux/amd64"}, io.Discard)
	if err == nil {
		t.Fatal("unguarded k8s.io import passed the check")
	}
	for _, want := range []string{"k8s.io/fake reaches the pod build", "example.com/m\n  -> example.com/m/kube\n  -> k8s.io/fake", "//go:build !nok8s"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}

	write("wire.go", "//go:build !nok8s\n\npackage main\n\nimport _ \"example.com/m/kube\"\n")
	if err := checkNoK8s(dir, ".", []string{"linux/amd64", "linux/arm64"}, io.Discard); err != nil {
		t.Errorf("guarded import failed the check: %v", err)
	}
}
