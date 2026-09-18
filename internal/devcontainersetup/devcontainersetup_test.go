package devcontainersetup

import (
	"strings"
	"testing"
)

func TestPromptNamesGitRemote(t *testing.T) {
	p := Prompt("git@github.com:org/repo.git")
	if !strings.Contains(p, "The repository to set up is git@github.com:org/repo.git.") {
		t.Fatalf("git prompt missing repository sentence: %q", p)
	}
	if strings.Contains(p, "LOCAL directory") {
		t.Fatalf("git prompt must not use the template wording: %q", p)
	}
}

// A template fleet's agent must work in the template dir itself — that is the
// directory fleet copies — rather than cloning something.
func TestPromptPointsTemplateAgentAtLocalDir(t *testing.T) {
	p := Prompt("file:///home/me/scratch")
	for _, want := range []string{"/home/me/scratch", "LOCAL directory", "skip the clone step"} {
		if !strings.Contains(p, want) {
			t.Fatalf("template prompt missing %q: %q", want, p)
		}
	}
	if strings.Contains(p, "file:///home/me/scratch") {
		t.Fatalf("template prompt should hand the agent a plain path, not the URL: %q", p)
	}
}
