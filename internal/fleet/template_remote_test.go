package fleet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsTemplateRemote(t *testing.T) {
	cases := map[string]bool{
		"file:///home/me/proj":          true,
		"  file:///home/me/proj ":       true,
		"file://localhost/home/me/proj": true,
		"git@github.com:org/repo.git":   false,
		"https://github.com/org/repo":   false,
		"/home/me/proj":                 false,
		"":                              false,
	}
	for remote, want := range cases {
		if got := IsTemplateRemote(remote); got != want {
			t.Errorf("IsTemplateRemote(%q) = %v, want %v", remote, got, want)
		}
	}
}

func TestTemplateDirAcceptsAbsoluteForms(t *testing.T) {
	cases := map[string]string{
		"file:///home/me/proj":          "/home/me/proj",
		"file:///home/me/proj/":         "/home/me/proj",
		"  file:///home/me/proj  ":      "/home/me/proj",
		"file://localhost/home/me/proj": "/home/me/proj",
		"file:///home/me/my%20dir":      "/home/me/my dir",
		"file:///home/me/a/../b":        "/home/me/b",
	}
	for remote, want := range cases {
		got, err := TemplateDir(remote)
		if err != nil {
			t.Errorf("TemplateDir(%q) error = %v", remote, err)
			continue
		}
		if got != want {
			t.Errorf("TemplateDir(%q) = %q, want %q", remote, got, want)
		}
	}
}

func TestTemplateDirRejectsNonAbsoluteAndNonTemplate(t *testing.T) {
	for _, remote := range []string{
		"file://proj",             // relative — missing third slash
		"file://host/proj",        // another host
		"file://",                 // nothing at all
		"file:///",                // filesystem root
		"git@github.com:o/r.git",  // not a template at all
		"https://example.com/x/y", // ditto
	} {
		if dir, err := TemplateDir(remote); err == nil {
			t.Errorf("TemplateDir(%q) = %q, want error", remote, dir)
		}
	}
	_, err := TemplateDir("file://proj")
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("relative path error should explain absolute requirement, got %v", err)
	}
}

func TestResolveTemplateDirChecksExistence(t *testing.T) {
	dir := t.TempDir()
	got, err := ResolveTemplateDir("file://" + dir)
	if err != nil || got != dir {
		t.Fatalf("ResolveTemplateDir(existing) = %q, %v", got, err)
	}
	if _, err := ResolveTemplateDir("file://" + filepath.Join(dir, "nope")); err == nil || !strings.Contains(err.Error(), "daemon's host") {
		t.Fatalf("missing dir: want stat error with the daemon-host hint, got %v", err)
	}
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveTemplateDir("file://" + file); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("file path: want not-a-directory error, got %v", err)
	}
	if _, err := ResolveTemplateDir("file://relative"); err == nil {
		t.Fatal("relative path must fail before any stat")
	}
}

func TestTemplateNameHintIsDirBase(t *testing.T) {
	if got := TemplateNameHint("file:///home/me/scratch-proj/"); got != "scratch-proj" {
		t.Errorf("TemplateNameHint = %q, want scratch-proj", got)
	}
	if got := TemplateNameHint("file://relative"); got != "" {
		t.Errorf("TemplateNameHint(invalid) = %q, want empty", got)
	}
}

// A template remote must NOT yield a derived fleet name: the user has to
// choose one (the TUI prompts, the CLI requires fleet/instance).
func TestFleetNameFromRemoteRefusesTemplate(t *testing.T) {
	if got := FleetNameFromRemote("file:///home/me/proj"); got != "" {
		t.Fatalf("FleetNameFromRemote(file://) = %q, want empty", got)
	}
	if got := FleetNameFromRemote("git@github.com:org/proj.git"); got != "proj" {
		t.Fatalf("git remote derivation regressed: %q", got)
	}
}

func TestResolveTemplateRepoRequiresExplicitFleet(t *testing.T) {
	_, err := Resolve("agent-1", "file:///home/me/proj")
	if err == nil {
		t.Fatal("expected error for a template --repo without fleet/instance")
	}
	if !strings.Contains(err.Error(), "<fleet>/agent-1") {
		t.Fatalf("error should show the fleet/instance form, got: %v", err)
	}

	target, err := Resolve("scratch/agent-1", "file:///home/me/proj")
	if err != nil {
		t.Fatalf("Resolve(fleet/instance, template): %v", err)
	}
	if target.Fleet != "scratch" || target.Instance != "agent-1" {
		t.Fatalf("target = %+v", target)
	}
}

// An explicit fleet/instance must win over --repo derivation (previously the
// --repo path ran first and left the slash inside the instance name).
func TestResolveExplicitFleetWinsOverRepoFlag(t *testing.T) {
	target, err := Resolve("mine/agent-1", "git@github.com:org/other.git")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.Fleet != "mine" || target.Instance != "agent-1" {
		t.Fatalf("target = %+v, want mine/agent-1", target)
	}
}

func TestValidateTemplateCreate(t *testing.T) {
	// Git remotes are never constrained here.
	if err := ValidateTemplateCreate("git@github.com:o/r.git", "feature/x", BackendCoder); err != nil {
		t.Fatalf("git remote: %v", err)
	}
	if err := ValidateTemplateCreate("file:///home/me/proj", "", BackendDevcontainer); err != nil {
		t.Fatalf("valid template: %v", err)
	}
	if err := ValidateTemplateCreate("file:///home/me/proj", "main", BackendDevcontainer); err == nil || !strings.Contains(err.Error(), "branch") {
		t.Fatalf("template + branch: want branch error, got %v", err)
	}
	for _, b := range []BackendType{BackendCoder, BackendCodespaces} {
		if err := ValidateTemplateCreate("file:///home/me/proj", "", b); err == nil || !strings.Contains(err.Error(), "devcontainer") {
			t.Fatalf("template + %s: want backend error, got %v", b, err)
		}
	}
	if err := ValidateTemplateCreate("file://relative", "", BackendDevcontainer); err == nil {
		t.Fatal("relative template path should be rejected")
	}
}
